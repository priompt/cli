package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Running from a subdirectory must resolve prompts against the repo root:
// URIs come from the repo path, so acme/support/agent.prompt seen as
// support/agent.prompt would publish under org "support".
func TestSubdirectoryPathsResolveAgainstRepoRoot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	if err := os.MkdirAll(filepath.Join("acme", "support"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("acme", "support", "agent.prompt"), []byte("Hi {name}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt.Add("acme")
	if _, err := wt.Commit("init", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t"}}); err != nil {
		t.Fatal(err)
	}

	t.Chdir(filepath.Join(dir, "acme", "support"))
	t.Cleanup(func() { cwdInRepo = "" })
	chdirRepoRoot()

	if got := filepath.ToSlash(cwdInRepo); got != "acme/support" {
		t.Fatalf("cwdInRepo = %q, want acme/support", got)
	}
	for _, arg := range []string{"agent.prompt", "./agent.prompt", filepath.Join(dir, "acme", "support", "agent.prompt")} {
		p := repoPath(arg)
		if p != "acme/support/agent.prompt" {
			t.Errorf("repoPath(%q) = %q, want acme/support/agent.prompt", arg, p)
		}
		if uri := pathToURI(p); uri != "priompt://acme/support/agent" {
			t.Errorf("pathToURI(%q) = %q, want priompt://acme/support/agent", p, uri)
		}
	}
	var out bytes.Buffer
	if v := runDiff(r, "HEAD", &out); len(v) != 0 || !bytes.Contains(out.Bytes(), []byte("no prompt changes")) {
		t.Errorf("diff from a subdirectory on a clean tree: verdicts %v, output %q", v, out.String())
	}
}
