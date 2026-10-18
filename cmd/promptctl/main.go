// Command promptctl is the prompt authoring CLI (Phase 3). Prompts live as
// *.prompt files in a git repo. Versioning, history and lineage ride on go-git
// (embedded — no external git binary needed, matching the server's single-binary
// ethos); promptctl adds the prompt-specific parts: validation, the *semantic
// propagation diff* (git's text diff can't express it), and env promotion.
//
//	promptctl commit -m "msg"            validate every prompt, then commit all changes
//	promptctl diff [-fail-on v] [ref]     semantic propagation diff vs ref (default HEAD)
//	promptctl log <file.prompt>          version lineage of one prompt
//	promptctl promote <file> <from> <to> bring a prompt from branch <from> onto <to>
//	promptctl push|pull                  validate (push) then sync with origin
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/priompt/proto/gen/priompt/v1"
	"github.com/priompt/proto/semdiff"
	"github.com/priompt/proto/validate"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "commit":
		commit(os.Args[2:])
	case "diff":
		diffCmd(os.Args[2:])
	case "log":
		logCmd(os.Args[2:])
	case "promote":
		promote(os.Args[2:])
	case "push":
		pushCmd(os.Args[2:])
	case "pull":
		pull()
	case "publish":
		publishCmd(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: promptctl commit|diff|log|promote|push|pull|publish [args]")
	os.Exit(2)
}

func commit(args []string) {
	fs := flag.NewFlagSet("commit", flag.ExitOnError)
	msg := fs.String("m", "", "commit message")
	fs.Parse(args)
	if *msg == "" {
		log.Fatal("commit needs -m \"message\"")
	}
	validateAll()
	r := openRepo()
	wt := worktree(r)
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		log.Fatal(err)
	}
	h, err := wt.Commit(*msg, &git.CommitOptions{Author: author(r)})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("committed %s\n", h.String()[:12])
}

// diffCmd runs the semantic propagation diff for every *.prompt that differs
// between ref (default HEAD) and the working tree. It exits 0 whatever the
// verdicts unless -fail-on names verdicts to gate on; then any prompt with one
// of them makes it exit 1 (after printing the full diff).
func diffCmd(args []string) {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	failOn := fs.String("fail-on", "", "exit 1 if any prompt's verdict is one of these (comma-separated: new, structural, tweak, minor)")
	fs.Parse(args)
	ref := "HEAD"
	if fs.NArg() > 0 {
		ref = fs.Arg(0)
		fs.Parse(fs.Args()[1:]) // flags may also follow the ref
	}
	gate, err := parseFailOn(*failOn)
	if err == nil && fs.NArg() > 0 {
		err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: promptctl diff [-fail-on verdicts] [ref]\n", err)
		os.Exit(2)
	}

	var gated []string
	for _, v := range runDiff(openRepo(), ref, os.Stdout) {
		if gate[v.verdict] {
			gated = append(gated, fmt.Sprintf("%s (%s)", v.path, v.verdict))
		}
	}
	if len(gated) > 0 {
		log.Fatalf("diff: %d prompt(s) match -fail-on %s: %s", len(gated), *failOn, strings.Join(gated, ", "))
	}
}

type fileVerdict struct{ path, verdict string }

// runDiff prints the diff of every changed prompt to w and returns each one's
// verdict: semdiff's worst hunk class, or "new" for a prompt absent from ref.
func runDiff(r *git.Repository, ref string, w io.Writer) []fileVerdict {
	tree := treeAt(r, ref)
	emb := semdiff.EmbedderFromEnv()

	var out []fileVerdict
	for _, p := range promptUnion(tree) {
		a, b, differs := changedLines(blobContent(tree, p), diskContent(p))
		if !differs {
			continue
		}
		if _, err := tree.File(p); err != nil { // added since ref: nothing to diff against
			fmt.Fprintf(w, "## %s\nnew prompt (not in %s)\n  => new\n\n", p, ref)
			out = append(out, fileVerdict{p, "new"})
			continue
		}
		res, err := semdiff.Analyze(emb, a, b)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Fprintf(w, "## %s\n%s\n", p, semdiff.Format(res))
		out = append(out, fileVerdict{p, semdiff.Worst(res)})
	}
	if len(out) == 0 {
		fmt.Fprintf(w, "no prompt changes vs %s\n", ref)
	}
	return out
}

// verdictNames maps each -fail-on name to the verdict it gates.
var verdictNames = map[string]string{
	"new": "new", "structural": "structural",
	"tweak": "localized tweak", "localized tweak": "localized tweak",
	"minor": "minor edit", "minor edit": "minor edit",
}

// parseFailOn turns a comma-separated -fail-on value into the set of verdicts
// that fail the diff. Empty means none (exit 0, the historical behavior).
func parseFailOn(s string) (map[string]bool, error) {
	set := map[string]bool{}
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		v, ok := verdictNames[name]
		if !ok {
			return nil, fmt.Errorf("-fail-on: unknown verdict %q (want new, structural, tweak, minor)", name)
		}
		set[v] = true
	}
	return set, nil
}

// logCmd prints a prompt's version lineage — every commit that touched it,
// following renames/forks.
func logCmd(args []string) {
	if len(args) != 1 {
		log.Fatal("usage: promptctl log <file.prompt>")
	}
	path := filepath.ToSlash(args[0])
	r := openRepo()
	iter, err := r.Log(&git.LogOptions{FileName: &path, Order: git.LogOrderCommitterTime})
	if err != nil {
		log.Fatal(err)
	}
	iter.ForEach(func(c *object.Commit) error {
		fmt.Printf("%s %s\n", c.Hash.String()[:8], firstLine(c.Message))
		return nil
	})
}

// promote brings one prompt from the `from` branch onto the `to` branch and
// commits it there — the dev → staging → prod version promotion. A no-op (the
// prompt already matches on `to`) reports "already up to date" instead of failing
// on an empty commit. The branch (or commit) checked out before is restored after.
// ponytail: assumes both branches exist and the tree is clean; go-git errors surface as-is.
func promote(args []string) {
	if len(args) != 3 {
		log.Fatal("usage: promptctl promote <file.prompt> <from-branch> <to-branch>")
	}
	path, from, to := filepath.ToSlash(args[0]), args[1], args[2]
	r := openRepo()

	content := blobContent(treeAt(r, from), path)
	if content == "" {
		log.Fatalf("prompt %q not found on branch %s", path, from)
	}
	if content == blobContent(treeAt(r, to), path) {
		fmt.Printf("already up to date: %s on %s matches %s\n", path, to, from)
		return
	}
	if err := validate.Prompt(pathToURI(path), content, deriveSlots(content)); err != nil {
		log.Fatalf("refusing to promote invalid prompt: %v", err)
	}

	wt := worktree(r)
	head, err := r.Head()
	if err != nil {
		log.Fatal(err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(to)}); err != nil {
		log.Fatalf("checkout %s: %v", to, err)
	}
	h, err := commitFile(r, wt, path, content, fmt.Sprintf("promote %s: %s -> %s", path, from, to))

	// Switch back to where the user was (their branch, or a detached commit),
	// whether or not the commit landed.
	back := &git.CheckoutOptions{Branch: head.Name()}
	if !head.Name().IsBranch() {
		back = &git.CheckoutOptions{Hash: head.Hash()}
	}
	if berr := wt.Checkout(back); berr != nil {
		log.Fatalf("checkout %s (to switch back from %s): %v", head.Name().Short(), to, berr)
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("promoted %s onto %s (%s)\n", path, to, h.String()[:12])
}

// commitFile writes content to path in the checked-out worktree and commits it.
func commitFile(r *git.Repository, wt *git.Worktree, path, content, msg string) (plumbing.Hash, error) {
	if err := os.WriteFile(filepath.FromSlash(path), []byte(content), 0o644); err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := wt.Add(path); err != nil {
		return plumbing.ZeroHash, err
	}
	return wt.Commit(msg, &git.CommitOptions{Author: author(r)})
}

// pushCmd validates, pushes to the git remote, and — if -server is given —
// publishes every prompt to that live server so subscribers are notified.
// Publishing is idempotent server-side, so only actually-changed prompts notify.
func pushCmd(args []string) {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	server := fs.String("server", "", "after pushing, publish prompts to this live server (host:port)")
	useTLS := fs.Bool("tls", false, "use TLS for -server")
	caCert := fs.String("ca-cert", "", "CA cert for TLS -server (default: system roots)")
	fs.Parse(args)

	validateAll()
	r := openRepo()
	reportSync("pushed", r.Push(&git.PushOptions{Auth: authFor(r), Progress: os.Stdout}))
	if *server != "" {
		publishToServer(*server, *useTLS, *caCert, diskPromptFiles())
	}
}

// publishCmd publishes prompts to a live server (and notifies subscribers)
// without touching the git remote. With no paths it publishes the whole tree.
func publishCmd(args []string) {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	server := fs.String("server", "", "live server address (host:port)")
	useTLS := fs.Bool("tls", false, "use TLS")
	caCert := fs.String("ca-cert", "", "CA cert for TLS (default: system roots)")
	fs.Parse(args)
	if *server == "" {
		log.Fatal("publish needs -server host:port")
	}
	paths := fs.Args()
	if len(paths) == 0 {
		paths = diskPromptFiles()
	}
	publishToServer(*server, *useTLS, *caCert, paths)
}

// publishToServer validates and PublishPrompts each prompt file to the server.
func publishToServer(server string, useTLS bool, caCert string, paths []string) {
	conn := dial(server, useTLS, caCert)
	defer conn.Close()
	client := pb.NewPromptServiceClient(conn)
	ctx := authCtx()
	for _, p := range paths {
		t := diskContent(p)
		slots := deriveSlots(t)
		if err := validate.Prompt(pathToURI(p), t, slots); err != nil {
			log.Fatalf("invalid %s: %v", p, err)
		}
		resp, err := client.PublishPrompt(ctx, &pb.PublishPromptRequest{Uri: pathToURI(p), Template: t, Slots: slots})
		if err != nil {
			log.Fatalf("publish %s: %v", p, err)
		}
		fmt.Printf("published %s (%s)\n", pathToURI(p), resp.GetVersionHash()[:12])
	}
}

func dial(addr string, useTLS bool, caCert string) *grpc.ClientConn {
	creds, err := transportCreds(useTLS, caCert)
	if err != nil {
		log.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		log.Fatal(err)
	}
	return conn
}

// transportCreds picks the gRPC transport: plaintext, TLS verified against the
// system root CAs, or TLS verified against the -ca-cert file.
func transportCreds(useTLS bool, caCert string) (credentials.TransportCredentials, error) {
	switch {
	case !useTLS:
		return insecure.NewCredentials(), nil
	case caCert == "":
		return credentials.NewTLS(&tls.Config{}), nil // nil RootCAs = system pool
	default:
		return credentials.NewClientTLSFromFile(caCert, "")
	}
}

func authCtx() context.Context {
	ctx := context.Background()
	if token := os.Getenv("PRIOMPT_TOKEN"); token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	return ctx
}

func pull() {
	r := openRepo()
	err := worktree(r).Pull(&git.PullOptions{Auth: authFor(r), Progress: os.Stdout})
	reportSync("pulled", err)
}

func reportSync(verb string, err error) {
	switch {
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		fmt.Println("already up to date")
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Println(verb)
	}
}

// --- prompt files ----------------------------------------------------------

var slotRe = regexp.MustCompile(`\{(\w+)\}`)

// deriveSlots reads declared slots straight from the template's {placeholders}.
// ponytail: derived, so the slot set always matches — add front-matter slot
// declarations if you want to catch typos against an intended set.
func deriveSlots(template string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range slotRe.FindAllStringSubmatch(template, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// pathToURI maps a repo-relative prompt path to its priompt:// URI.
func pathToURI(p string) string {
	return "priompt://" + strings.TrimSuffix(filepath.ToSlash(p), ".prompt")
}

func validateAll() {
	bad := 0
	for _, p := range diskPromptFiles() {
		t := diskContent(p)
		if err := validate.Prompt(pathToURI(p), t, deriveSlots(t)); err != nil {
			fmt.Fprintf(os.Stderr, "invalid %s: %v\n", p, err)
			bad++
		}
	}
	if bad > 0 {
		log.Fatalf("%d invalid prompt(s); nothing committed", bad)
	}
}

// diskPromptFiles walks the working tree (run from the repo root) for *.prompt
// files, returned as slash paths.
func diskPromptFiles() []string {
	var out []string
	filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".prompt") {
			out = append(out, filepath.ToSlash(p))
		}
		return nil
	})
	return out
}

// promptUnion is the sorted set of *.prompt paths present in the ref tree or on
// disk — so diff also catches added and deleted prompts.
func promptUnion(tree *object.Tree) []string {
	set := map[string]bool{}
	tree.Files().ForEach(func(f *object.File) error {
		if strings.HasSuffix(f.Name, ".prompt") {
			set[f.Name] = true
		}
		return nil
	})
	for _, p := range diskPromptFiles() {
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// --- go-git helpers --------------------------------------------------------

func openRepo() *git.Repository {
	r, err := git.PlainOpenWithOptions(".", &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		log.Fatalf("not a git repo: %v", err)
	}
	return r
}

func worktree(r *git.Repository) *git.Worktree {
	wt, err := r.Worktree()
	if err != nil {
		log.Fatal(err)
	}
	return wt
}

func treeAt(r *git.Repository, ref string) *object.Tree {
	h, err := r.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		log.Fatalf("unknown ref %q: %v", ref, err)
	}
	c, err := r.CommitObject(*h)
	if err != nil {
		log.Fatal(err)
	}
	t, err := c.Tree()
	if err != nil {
		log.Fatal(err)
	}
	return t
}

// blobContent returns the file's content in a tree, or "" if it isn't there.
func blobContent(tree *object.Tree, slashPath string) string {
	f, err := tree.File(slashPath)
	if err != nil {
		return ""
	}
	c, err := f.Contents()
	if err != nil {
		log.Fatal(err)
	}
	return c
}

func diskContent(slashPath string) string {
	b, err := os.ReadFile(filepath.FromSlash(slashPath))
	if err != nil {
		return "" // absent on disk (deleted, or only exists in the ref)
	}
	return string(b)
}

// author resolves the commit identity from git config (local overrides global),
// falling back to a generic promptctl identity.
func author(r *git.Repository) *object.Signature {
	name, email := "promptctl", "promptctl@localhost"
	for _, sc := range []config.Scope{config.SystemScope, config.GlobalScope, config.LocalScope} {
		if cfg, err := r.ConfigScoped(sc); err == nil {
			if cfg.User.Name != "" {
				name = cfg.User.Name
			}
			if cfg.User.Email != "" {
				email = cfg.User.Email
			}
		}
	}
	return &object.Signature{Name: name, Email: email, When: time.Now()}
}

// authFor picks remote credentials: PRIOMPT_GIT_TOKEN for HTTPS, ssh-agent for
// SSH. ponytail: go-git ignores git's credential helper, so these two cover the
// common cases; wire a keyfile method if neither fits.
func authFor(r *git.Repository) transport.AuthMethod {
	rem, err := r.Remote("origin")
	if err != nil || len(rem.Config().URLs) == 0 {
		return nil
	}
	url := rem.Config().URLs[0]
	if strings.HasPrefix(url, "http") {
		if tok := os.Getenv("PRIOMPT_GIT_TOKEN"); tok != "" {
			return &githttp.BasicAuth{Username: "git", Password: tok}
		}
		return nil
	}
	if a, err := gitssh.NewSSHAgentAuth("git"); err == nil {
		return a
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func splitLines(s string) []string {
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

// changedLines splits both versions into lines and reports whether they differ.
// The comparison runs after splitLines normalises line endings, so a CRLF-only
// difference (core.autocrlf on Windows) is not a change.
func changedLines(old, neu string) (a, b []string, changed bool) {
	a, b = splitLines(old), splitLines(neu)
	return a, b, !slices.Equal(a, b)
}
