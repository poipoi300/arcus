package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func needGit(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("git is not on PATH")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCommitRecordsChangesOnly(t *testing.T) {
	needGit(t)
	t.Setenv("HOME", t.TempDir()) // no identity or config of the developer
	dir := t.TempDir()
	write(t, filepath.Join(dir, "host.toml"), "modules = [\"a\"]\n")
	write(t, filepath.Join(dir, ".lock"), "")
	write(t, filepath.Join(dir, "ledger", ".a.json.123.tmp"), "torn")
	write(t, filepath.Join(dir, "backups", "orig", "f.arcus-tmp"), "partial")

	if err := Commit(dir, "on a"); err != nil {
		t.Fatal(err)
	}
	if err := Commit(dir, "nothing changed"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "host.toml"), "modules = []\n")
	if err := Commit(dir, "off a"); err != nil {
		t.Fatal(err)
	}

	log, err := Log(dir, 20)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " off a") || !strings.HasSuffix(lines[1], " on a") {
		t.Errorf("log:\n%s", log)
	}
	tracked, err := git(dir, "ls-files")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(tracked)); len(got) != 2 || got[0] != ".gitignore" || got[1] != "host.toml" {
		t.Errorf("tracked = %v, want .gitignore and host.toml", got)
	}

	rev := strings.Fields(lines[1])[0]
	old, err := Show(dir, rev, "host.toml")
	if err != nil || string(old) != "modules = [\"a\"]\n" {
		t.Errorf("Show = %q, %v", old, err)
	}
	if _, err := Show(dir, "nope", "host.toml"); err == nil {
		t.Error("Show of an unknown revision succeeded")
	}
	if _, err := Show(dir, "--help", "host.toml"); err == nil {
		t.Error("Show of an option succeeded")
	}
}

func TestCommitNeverCreatesTheDirectory(t *testing.T) {
	needGit(t)
	dir := filepath.Join(t.TempDir(), "state")
	if err := Commit(dir, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("Commit created the state directory")
	}
	if _, err := Log(dir, 5); err != ErrNoRepo {
		t.Errorf("Log of a missing repository: %v", err)
	}
}

func TestWithoutGitNothingHappens(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	if err := Commit(dir, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Error("a repository appeared without git")
	}
	if _, err := Log(dir, 5); err != ErrNoGit {
		t.Errorf("Log without git: %v", err)
	}
}
