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

// publishRepo makes a repo with acme/a.prompt committed on prod and dev
// (different content), checked out on the given branch, and chdirs into it.
func publishRepo(t *testing.T, branch string) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	sig := &object.Signature{Name: "t", Email: "t@t"}
	commit := func(content, msg string) {
		t.Helper()
		os.MkdirAll("acme", 0o755)
		if err := os.WriteFile("acme/a.prompt", []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		wt.Add("acme/a.prompt")
		if _, err := wt.Commit(msg, &git.CommitOptions{Author: sig}); err != nil {
			t.Fatal(err)
		}
	}
	commit("A prod {x}\n", "ship a\n\nbody")
	head, _ := r.Head()
	for _, b := range []string{"prod", "dev"} {
		r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), head.Hash()))
	}
	wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("dev")})
	commit("A dev {x}\n", "dev work")
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch)}); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPublishSendsCommittedContentWithMessage(t *testing.T) {
	r := publishRepo(t, "prod")
	plan := committedPublications(r, []string{"acme/a.prompt"}, "")
	if len(plan) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	p := plan[0]
	if p.template != "A prod {x}\n" || p.branch != "" || !strings.HasPrefix(p.message, "ship a (") {
		t.Errorf("publication = %+v, want prod's committed content, server main, HEAD subject as message", p)
	}
}

func TestPublishFromDevNeedsExplicitBranch(t *testing.T) {
	r := publishRepo(t, "dev")
	plan := committedPublications(r, []string{"acme/a.prompt"}, "dev")
	if plan[0].branch != "dev" || plan[0].template != "A dev {x}\n" {
		t.Errorf("with -branch dev: %+v, want dev content on server branch dev", plan[0])
	}

	out, failed := runPublishCase(t, "dev-to-main")
	if !failed || !strings.Contains(out, "non-serving branch") {
		t.Errorf("dev without -branch: failed=%v out=%q, want refusal", failed, out)
	}
}

// A CRLF checkout of an LF blob (core.autocrlf on Windows) is not an
// uncommitted change, even when go-git's status reports it as modified.
func TestPublishIgnoresLineEndingOnlyDifferences(t *testing.T) {
	r := publishRepo(t, "prod")
	if err := os.WriteFile("acme/a.prompt", []byte("A prod {x}\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := committedPublications(r, []string{"acme/a.prompt"}, "")
	if plan[0].template != "A prod {x}\n" {
		t.Errorf("template = %q, want the committed LF content", plan[0].template)
	}
}

func TestPublishRefusesUncommittedChanges(t *testing.T) {
	out, failed := runPublishCase(t, "dirty-prod")
	if !failed || !strings.Contains(out, "uncommitted changes") {
		t.Errorf("dirty prompt on prod: failed=%v out=%q, want refusal", failed, out)
	}
}

// runPublishCase runs one refusal scenario in a child process, since
// committedPublications exits via log.Fatal.
func runPublishCase(t *testing.T, name string) (string, bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPublishHelperProcess$")
	cmd.Env = append(os.Environ(), "PROMPTCTL_PUBLISH_CASE="+name)
	out, err := cmd.CombinedOutput()
	return string(out), err != nil
}

func TestPublishHelperProcess(t *testing.T) {
	switch os.Getenv("PROMPTCTL_PUBLISH_CASE") {
	case "dev-to-main":
		committedPublications(publishRepo(t, "dev"), []string{"acme/a.prompt"}, "")
	case "dirty-prod":
		r := publishRepo(t, "prod")
		os.WriteFile("acme/a.prompt", []byte("A UNCOMMITTED {x}\n"), 0o644)
		committedPublications(r, []string{"acme/a.prompt"}, "")
	default:
		t.Skip("helper for runPublishCase")
	}
	os.Exit(0)
}
