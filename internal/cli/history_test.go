package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/grunclepug/arcus/internal/history"
)

func needGit(t *testing.T) {
	t.Helper()
	if !history.Available() {
		t.Skip("git is not on PATH")
	}
}

func (e *env) userState() string { return filepath.Join(e.home, ".local", "state", "arcus") }

func (e *env) systemState() string { return filepath.Join(e.root, "var", "lib", "arcus") }

// commits returns the messages of a state directory's history, newest first.
func (e *env) commits(dir string) []string {
	e.t.Helper()
	out, err := history.Log(dir, 100)
	if err != nil {
		return nil
	}
	var msgs []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if fields := strings.SplitN(line, " ", 3); len(fields) == 3 {
			msgs = append(msgs, fields[2])
		}
	}
	return msgs
}

// rev returns the abbreviated hash of the commit with message msg.
func (e *env) rev(dir, msg string) string {
	e.t.Helper()
	out, err := history.Log(dir, 100)
	must(e.t, err)
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if fields := strings.SplitN(line, " ", 3); len(fields) == 3 && fields[2] == msg {
			return fields[0]
		}
	}
	e.t.Fatalf("no commit %q in:\n%s", msg, out)
	return ""
}

func gitFiles(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "ls-files").Output()
	must(t, err)
	return strings.Fields(string(out))
}

func TestStateChangesAreRecordedPerHalf(t *testing.T) {
	needGit(t)
	e := newEnv(t)
	must(t, os.WriteFile(filepath.Join(e.home, ".bashrc"), []byte("old"), 0o644))
	must(t, os.WriteFile(filepath.Join(e.root, "etc/p.conf"), []byte("host"), 0o644))
	e.module("p", "", map[string]string{"home/.bashrc": "new", "system/etc/p.conf": "AAAA"})

	must(t, e.run("on", "-y", "p"))
	for _, dir := range []string{e.userState(), e.systemState()} {
		if got := e.commits(dir); !slices.Equal(got, []string{"on -y p"}) {
			t.Errorf("%s: commits = %q", dir, got)
		}
	}
	user := gitFiles(t, e.userState())
	for _, want := range []string{".gitignore", "host.toml", "state.json", "ledger/p.json", "backups/orig/.bashrc"} {
		if !slices.Contains(user, want) {
			t.Errorf("user history lacks %s: %v", want, user)
		}
	}
	if slices.Contains(user, ".lock") {
		t.Error("the lock file is tracked")
	}
	if system := gitFiles(t, e.systemState()); !slices.Contains(system, "backups/orig/etc/p.conf") {
		t.Errorf("system history lacks the backup: %v", system)
	}

	must(t, os.WriteFile(filepath.Join(e.root, "etc/p.conf"), []byte("BBBB"), 0o644))
	must(t, e.run("off", "-f", "-y", "p"))
	if got := e.commits(e.systemState()); got[0] != "off -f -y p" || len(got) != 2 {
		t.Errorf("system commits = %q", got)
	}
	if system := gitFiles(t, e.systemState()); slices.Contains(system, "foreign/etc/p.conf") || slices.Contains(system, "backups/orig/etc/p.conf") {
		t.Errorf("system history after off -f: %v (foreign/ is not tracked)", system)
	}
	if e.read("var/lib/arcus/foreign/etc/p.conf") != "BBBB" {
		t.Error("the foreign file is gone from disk")
	}
}

func TestReadOnlyCommandsRecordNothing(t *testing.T) {
	needGit(t)
	e := newEnv(t)
	e.module("p", "", map[string]string{"home/.bashrc": "new", "system/etc/p.conf": "AAAA"})
	for _, args := range [][]string{{"plan", "p"}, {"on", "-n", "p"}} {
		must(t, e.run(args...))
	}
	if _, err := os.Stat(e.userState()); err == nil {
		if _, err := os.Stat(filepath.Join(e.userState(), ".git")); err == nil {
			t.Fatal("a dry run started a history")
		}
	}

	must(t, e.run("on", "-y", "p"))
	before := e.commits(e.userState())
	for _, args := range [][]string{{"ls"}, {"plan"}, {"doctor"}, {"apply", "-n"}, {"history"}, {"history", "-system"}} {
		must(t, e.run(args...))
	}
	if got := e.commits(e.userState()); !slices.Equal(got, before) {
		t.Errorf("read-only commands changed the history: %q", got)
	}
}

func TestHistoryCommand(t *testing.T) {
	needGit(t)
	e := newEnv(t)
	e.module("p", "", map[string]string{"home/.bashrc": "new", "system/etc/p.conf": "AAAA"})
	e.module("q", "", map[string]string{"home/.inputrc": "q"})

	must(t, e.run("history"))
	if !strings.Contains(e.out.String(), "No history") {
		t.Errorf("history before any change:\n%s", e.out.String())
	}
	must(t, e.run("on", "-y", "p"))
	must(t, e.run("on", "-y", "q"))
	must(t, e.run("history", "-n", "1"))
	if lines := strings.Split(strings.TrimSpace(e.out.String()), "\n"); len(lines) != 1 || !strings.HasSuffix(lines[0], " on -y q") {
		t.Errorf("history -n 1:\n%s", e.out.String())
	}
	must(t, e.run("history"))
	if lines := strings.Split(strings.TrimSpace(e.out.String()), "\n"); len(lines) != 2 {
		t.Errorf("history:\n%s", e.out.String())
	}
	must(t, e.run("history", "-system"))
	if lines := strings.Split(strings.TrimSpace(e.out.String()), "\n"); len(lines) != 1 || !strings.HasSuffix(lines[0], " on -y p") {
		t.Errorf("history -system (q has no system files):\n%s", e.out.String())
	}
	if err := e.run("history", "-n", "0"); err == nil {
		t.Error("history -n 0 succeeded")
	}
}

func TestRollbackRestoresTheHostFile(t *testing.T) {
	needGit(t)
	e := newEnv(t)
	e.module("p", "", map[string]string{"home/.bashrc": "p"})
	e.module("q", "", map[string]string{"home/.inputrc": "q"})
	must(t, e.run("on", "-y", "p"))
	must(t, e.run("on", "-y", "q"))
	rev := e.rev(e.userState(), "on -y p")

	// Declining puts nothing back and changes nothing.
	e.out.Reset()
	if err := e.app("n\n").run([]string{"rollback", rev}); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("declined rollback: %v", err)
	}
	if !strings.Contains(e.out.String(), "- module q") {
		t.Errorf("no diff shown:\n%s", e.out.String())
	}
	if !slices.Equal(e.hostModules(), []string{"p", "q"}) || e.read("home/u/.inputrc") == "<missing>" {
		t.Fatal("a declined rollback changed something")
	}

	data, err := os.ReadFile(e.hostPath())
	must(t, err)
	must(t, os.WriteFile(e.hostPath(), append(data, "# edited by hand\n"...), 0o644))
	e.out.Reset()
	must(t, e.app("y\n").run([]string{"rollback", rev}))
	if !slices.Equal(e.hostModules(), []string{"p"}) {
		t.Errorf("host modules = %v", e.hostModules())
	}
	if _, err := os.Lstat(filepath.Join(e.home, ".inputrc")); err == nil {
		t.Error("q is still deployed")
	}
	if got := e.commits(e.userState()); got[0] != "rollback "+rev || got[1] != "before rollback "+rev {
		t.Errorf("commits = %q", got)
	}
	if kept, err := history.Show(e.userState(), e.rev(e.userState(), "before rollback "+rev), "host.toml"); err != nil || !strings.Contains(string(kept), "edited by hand") {
		t.Errorf("the hand edit was not recorded: %q, %v", kept, err)
	}

	if err := e.run("rollback", "nonsense"); err == nil || !strings.Contains(err.Error(), "not a revision") {
		t.Errorf("rollback to nonsense: %v", err)
	}
	if err := e.run("rollback"); err == nil {
		t.Error("rollback without a revision succeeded")
	}
}

func TestDoctorNotesMissingGit(t *testing.T) {
	e := newEnv(t)
	t.Setenv("PATH", t.TempDir()) // no git
	var buf strings.Builder
	e.app("").doctorHistory(&report{w: &buf})
	if !strings.Contains(buf.String(), "git not found: state history is off") {
		t.Errorf("doctor note: %q", buf.String())
	}
	if err := e.app("").run([]string{"history"}); err == nil || !strings.Contains(err.Error(), "git not found") {
		t.Errorf("history without git: %v", err)
	}
}
