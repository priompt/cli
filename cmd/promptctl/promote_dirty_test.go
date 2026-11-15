package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// promoteRepo makes a repo on master whose acme/a.prompt differs on dev, with
// acme/b.prompt committed everywhere, and chdirs into it.
func promoteRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	sig := &object.Signature{Name: "t", Email: "t@t"}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll("acme", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("acme/a.prompt", "A v1 {x}\n")
	write("acme/b.prompt", "B v1 {y}\n")
	wt.Add("acme")
	if _, err := wt.Commit("init", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	head, _ := r.Head()
	for _, b := range []string{"dev", "prod"} {
		r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), head.Hash()))
	}
	wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("dev")})
	write("acme/a.prompt", "A v2 {x}\n")
	wt.Add("acme/a.prompt")
	if _, err := wt.Commit("dev edit", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("master")}); err != nil {
		t.Fatal(err)
	}
	return r
}

// runPromote runs promote in a child process (it exits via log.Fatal) and
// returns its combined output and whether it failed.
func runPromote(t *testing.T, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPromoteHelperProcess$")
	cmd.Env = append(os.Environ(), "PROMPTCTL_PROMOTE_ARGS="+strings.Join(args, ","))
	out, err := cmd.CombinedOutput()
	return string(out), err != nil
}

func TestPromoteHelperProcess(t *testing.T) {
	args := os.Getenv("PROMPTCTL_PROMOTE_ARGS")
	if args == "" {
		t.Skip("helper for runPromote")
	}
	promote(strings.Split(args, ","))
	os.Exit(0)
}

func TestPromoteRefusesStagedChanges(t *testing.T) {
	r := promoteRepo(t)
	wt, _ := r.Worktree()
	os.WriteFile("acme/b.prompt", []byte("B staged {y}\n"), 0o644)
	os.WriteFile("acme/n.prompt", []byte("N new {n}\n"), 0o644)
	wt.Add("acme")

	out, failed := runPromote(t, "acme/a.prompt", "dev", "prod")
	if !failed || !strings.Contains(out, "clean working tree") {
		t.Fatalf("promote with staged changes: failed=%v out=%q, want refusal", failed, out)
	}
	if got := diskContent("acme/b.prompt"); got != "B staged {y}\n" {
		t.Errorf("staged edit lost: b.prompt = %q", got)
	}
	if got := diskContent("acme/n.prompt"); got != "N new {n}\n" {
		t.Errorf("staged new file lost: n.prompt = %q", got)
	}
	if st, _ := wt.Status(); st.File("acme/n.prompt").Staging != git.Added {
		t.Errorf("n.prompt no longer staged: %q", st.File("acme/n.prompt").Staging)
	}
	if got := blobContent(treeAt(r, "prod"), "acme/a.prompt"); got != "A v1 {x}\n" {
		t.Errorf("prod changed to %q although promote refused", got)
	}
}

func TestPromoteRefusesUnstagedChangesAndStaysOnBranch(t *testing.T) {
	r := promoteRepo(t)
	os.WriteFile("acme/b.prompt", []byte("B wip {y}\n"), 0o644)

	out, failed := runPromote(t, "acme/a.prompt", "dev", "prod")
	if !failed || !strings.Contains(out, "clean working tree") {
		t.Fatalf("promote with unstaged changes: failed=%v out=%q, want refusal", failed, out)
	}
	if h, _ := r.Head(); h.Name().Short() != "master" {
		t.Errorf("HEAD is %s after refused promote, want master", h.Name().Short())
	}
	if got := diskContent("acme/b.prompt"); got != "B wip {y}\n" {
		t.Errorf("unstaged edit lost: b.prompt = %q", got)
	}
}

// A CRLF checkout of an LF blob (core.autocrlf on Windows) is not a change,
// even when go-git's status reports it as modified.
func TestPromoteAllowsLineEndingOnlyDifferences(t *testing.T) {
	r := promoteRepo(t)
	os.WriteFile("acme/b.prompt", []byte("B v1 {y}\r\n"), 0o644)

	promote([]string{"acme/a.prompt", "dev", "prod"})

	if got := blobContent(treeAt(r, "prod"), "acme/a.prompt"); got != "A v2 {x}\n" {
		t.Errorf("prod has %q, want promoted content", got)
	}
}

func TestPromoteAllowsUntrackedFiles(t *testing.T) {
	r := promoteRepo(t)
	os.WriteFile("notes.txt", []byte("scratch"), 0o644)

	promote([]string{"acme/a.prompt", "dev", "prod"})

	if got := blobContent(treeAt(r, "prod"), "acme/a.prompt"); got != "A v2 {x}\n" {
		t.Errorf("prod has %q, want promoted content", got)
	}
	if b, err := os.ReadFile("notes.txt"); err != nil || string(b) != "scratch" {
		t.Errorf("untracked file lost: %q %v", b, err)
	}
	if h, _ := r.Head(); h.Name().Short() != "master" {
		t.Errorf("HEAD is %s, want master", h.Name().Short())
	}
}
