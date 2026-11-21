package layer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stored reads a file in the sandbox's state directory, or "<missing>".
func (sb *sandbox) stored(rel string) string {
	data, err := os.ReadFile(filepath.Join(sb.s.StateDir, rel))
	if err != nil {
		return "<missing>"
	}
	return string(data)
}

// legacy turns the store backups of module's ledger into the sidecar files
// older versions left next to the host, so the ledger looks like theirs.
func (sb *sandbox) legacy(module, tag string) {
	sb.t.Helper()
	l, err := sb.s.LoadLedger(module)
	must(sb.t, err)
	for i := range l.Entries {
		e := &l.Entries[i]
		if e.Backup == "" {
			continue
		}
		sidecar := uniquePath(e.Host + "." + tag)
		must(sb.t, os.Rename(e.Backup, sidecar))
		e.Backup = sidecar
	}
	must(sb.t, sb.s.saveLedger(l))
}

func TestBackupsLiveInTheStateDir(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("deep/dir/f", "ORIGINAL")
		sb.moduleFile("a", "deep/dir/f", "a")
		before := sb.snapshot()

		must(t, sb.activate("a"))
		want := filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "deep", "dir", "f")
		l, err := sb.s.LoadLedger("a")
		must(t, err)
		if l.Entries[0].Backup != want || sb.stored("backups/orig/etc/deep/dir/f") != "ORIGINAL" {
			t.Fatalf("backup = %q, want %q", l.Entries[0].Backup, want)
		}
		if matches, _ := filepath.Glob(filepath.Join(sb.host, "deep/dir/*")); len(matches) != 1 {
			t.Errorf("sidecar files next to the host: %v", matches)
		}

		must(t, sb.deactivate("a", false))
		assertSame(t, before, sb.snapshot())
		// The emptied directories are gone; the tag directory stays.
		if exists(filepath.Join(sb.s.StateDir, "backups", "orig", "etc")) || !exists(filepath.Join(sb.s.StateDir, "backups", "orig")) {
			t.Error("store directories were not pruned up to the tag directory")
		}
	})
}

// With B over A, B's backup holds A's deployment in backups/a. Taking A off
// splices B onto A's own backup.
func TestSpliceWithBackupsInTheStore(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("a", "f", "a")
		sb.moduleFile("b", "f", "b")
		before := sb.snapshot()
		must(t, sb.activate("a"))
		must(t, sb.activate("b"))
		if sb.stored("backups/orig/etc/f") != "ORIGINAL" || !exists(filepath.Join(sb.s.StateDir, "backups", "a", "etc", "f")) {
			t.Fatalf("store: orig=%q", sb.stored("backups/orig/etc/f"))
		}

		must(t, sb.deactivate("a", false))
		if sb.s.Copy && sb.read("f") != "b" {
			t.Fatalf("top layer disturbed: %q", sb.read("f"))
		}
		if exists(filepath.Join(sb.s.StateDir, "backups", "a", "etc")) {
			t.Error("A's backup directory was not pruned")
		}
		lb, _ := sb.s.LoadLedger("b")
		if lb.Entries[0].Backup != filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "f") {
			t.Errorf("B was not spliced onto the original: %q", lb.Entries[0].Backup)
		}

		must(t, sb.deactivate("b", false))
		assertSame(t, before, sb.snapshot())
		if dangling, err := sb.s.DanglingBackups(); err != nil || len(dangling) != 0 {
			t.Errorf("dangling = %v, %v", dangling, err)
		}
	})
}

func TestForeignFilesAreMovedIntoTheStateDir(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("d/g.cfg", "ORIGINAL")
		sb.moduleFile("g", "d/g.cfg", "AAAA")
		for i, edit := range []string{"BBBB", "CCCC"} {
			must(t, sb.activate("g"))
			p := filepath.Join(sb.host, "d/g.cfg")
			must(t, os.Remove(p))
			must(t, os.WriteFile(p, []byte(edit), 0o644))
			sb.warns = nil
			must(t, sb.deactivate("g", true))

			want := filepath.Join(sb.s.StateDir, "foreign", "etc", "d", "g.cfg")
			if i == 1 {
				want += ".1"
			}
			if got, _ := os.ReadFile(want); string(got) != edit {
				t.Errorf("%s = %q, want %q", want, got, edit)
			}
			if len(sb.warns) != 1 || !strings.Contains(sb.warns[0], want) {
				t.Errorf("warning must name %s: %v", want, sb.warns)
			}
			if matches, _ := filepath.Glob(filepath.Join(sb.host, "d", "*")); len(matches) != 1 {
				t.Errorf("sidecar files next to the host: %v", matches)
			}
		}
	})
}

// A ledger written before backups moved names a sidecar file next to the
// host; it is spliced and restored from there.
func TestLegacySidecarBackupStillRestores(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("a", "f", "a")
		sb.moduleFile("b", "f", "b")
		before := sb.snapshot()
		must(t, sb.activate("a"))
		must(t, sb.activate("b"))
		sb.legacy("a", "orig")
		sb.legacy("b", "a")
		if sb.read("f.orig") != "ORIGINAL" {
			t.Fatalf("setup: %q", sb.read("f.orig"))
		}

		must(t, sb.deactivate("a", false))
		must(t, sb.deactivate("b", false))
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()
	})
}

func TestRenameMovesStoreAndLegacyBackups(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		eachHalf(t, func(t *testing.T, sb *sandbox) {
			sb.hostFile("f", "ORIGINAL")
			sb.moduleFile("old", "f", "old")
			sb.moduleFile("top", "f", "top")
			must(t, sb.activate("old"))
			must(t, sb.activate("top"))
			if legacy {
				sb.legacy("top", "old")
			}

			oldDir, newDir := sb.s.Dirs["old"], filepath.Join(sb.profDir, "new")
			must(t, os.Rename(oldDir, newDir))
			delete(sb.s.Dirs, "old")
			sb.s.Dirs["new"] = newDir
			must(t, sb.s.Rename("old", "new", oldDir, newDir, sb.stack))
			sb.stack = []string{"new", "top"}

			l, _ := sb.s.LoadLedger("top")
			want := filepath.Join(sb.s.StateDir, "backups", "new", "etc", "f")
			if legacy {
				want = filepath.Join(sb.host, "f.new")
			}
			if l.Entries[0].Backup != want || !exists(want) {
				t.Errorf("legacy=%v: backup = %q, want %q", legacy, l.Entries[0].Backup, want)
			}
			if !legacy && exists(filepath.Join(sb.s.StateDir, "backups", "old")) {
				t.Error("the old backup directory was not pruned")
			}
			must(t, sb.deactivate("new", false))
			must(t, sb.deactivate("top", false))
			if sb.read("f") != "ORIGINAL" {
				t.Errorf("legacy=%v: f = %q", legacy, sb.read("f"))
			}
		})
	}
}

func TestDanglingBackupsInTheStore(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("f", "x")
		sb.moduleFile("p", "f", "p")
		must(t, sb.activate("p"))
		stray := filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "stray")
		must(t, os.MkdirAll(filepath.Dir(stray), 0o755))
		must(t, os.WriteFile(stray, []byte("left by a crash"), 0o644))

		got, err := sb.s.DanglingBackups()
		must(t, err)
		if len(got) != 1 || got[0] != stray {
			t.Fatalf("dangling = %v, want only %s", got, stray)
		}
	})
}

// The cross-filesystem path of moveDurable, called directly: a test can't
// force EXDEV.
func TestCopyDurable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	must(t, os.WriteFile(src, []byte("content"), 0o640))
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, os.Chtimes(src, mtime, mtime))
	must(t, os.Symlink("target", filepath.Join(dir, "link")))
	must(t, syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644))

	out := filepath.Join(dir, "out")
	must(t, os.Mkdir(out, 0o755))
	must(t, copyDurable(src, filepath.Join(out, "dst")))
	info, err := os.Lstat(filepath.Join(out, "dst"))
	must(t, err)
	if data, _ := os.ReadFile(filepath.Join(out, "dst")); string(data) != "content" || info.Mode().Perm() != 0o640 || !info.ModTime().Equal(mtime) {
		t.Errorf("copy = %q %v %v", data, info.Mode(), info.ModTime())
	}
	if st := info.Sys().(*syscall.Stat_t); int(st.Uid) != os.Getuid() {
		t.Errorf("owner = %d, want %d", st.Uid, os.Getuid())
	}

	must(t, copyDurable(filepath.Join(dir, "link"), filepath.Join(out, "link")))
	if got, err := os.Readlink(filepath.Join(out, "link")); err != nil || got != "target" {
		t.Errorf("link = %q, %v", got, err)
	}

	if err := copyDurable(filepath.Join(dir, "fifo"), filepath.Join(out, "fifo")); err == nil {
		t.Error("a fifo must be refused")
	}
	if entries, _ := os.ReadDir(out); len(entries) != 2 {
		t.Errorf("files in out/ = %v, want only dst and link", entries)
	}
}

func TestMoveDurable(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644))
	must(t, moveDurable(filepath.Join(dir, "a"), filepath.Join(dir, "b")))
	if exists(filepath.Join(dir, "a")) || !exists(filepath.Join(dir, "b")) {
		t.Error("file was not moved")
	}
	if err := moveDurable(filepath.Join(dir, "missing"), filepath.Join(dir, "c")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("moving a missing file: %v", err)
	}
}

// A crash between the copy into place and the removal of the source leaves
// both; turning the module off converges either way.
func TestInterruptedCopyMoveConverges(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("p", "f", "p")
		before := sb.snapshot()
		must(t, sb.activate("p"))

		// Teardown crashed after the backup was copied back to the host.
		l, err := sb.s.LoadLedger("p")
		must(t, err)
		must(t, os.Remove(l.Entries[0].Host))
		must(t, copyDurable(l.Entries[0].Backup, l.Entries[0].Host))
		findings, err := sb.s.Audit("p", sb.stack)
		must(t, err)
		if len(findings) == 0 || !strings.Contains(findings[0], "duplicate") {
			t.Errorf("audit of the duplicate: %v", findings)
		}

		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()
		if dangling, _ := sb.s.DanglingBackups(); len(dangling) != 0 {
			t.Errorf("dangling after convergence: %v", dangling)
		}
	})
}

// A directory where a file was deployed can't be copied to another
// filesystem; -f leaves it next to the host and says so.
func TestForeignDirectoryStaysInPlaceAcrossFilesystems(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.hostFile("g", "ORIGINAL")
		sb.moduleFile("p", "g", "p")
		must(t, sb.activate("p"))
		p := filepath.Join(sb.host, "g")
		must(t, os.Remove(p))
		must(t, os.MkdirAll(filepath.Join(p, "sub"), 0o755))
		must(t, os.WriteFile(filepath.Join(p, "sub", "mine"), []byte("mine"), 0o644))

		must(t, sb.deactivate("p", true))
		if sb.read("g") != "ORIGINAL" || sb.read("g.arcus-foreign/sub/mine") != "mine" {
			t.Errorf("g = %q, aside = %q", sb.read("g"), sb.read("g.arcus-foreign/sub/mine"))
		}
		if len(sb.warns) != 1 || !strings.Contains(sb.warns[0], "stays here") {
			t.Errorf("warning = %v", sb.warns)
		}
	})
}

func TestGitDirectoriesAreRefused(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.moduleFile("p", "proj/.git/config", "x")
		err := sb.activate("p")
		if err == nil || !strings.Contains(err.Error(), "may not deploy into a .git directory") {
			t.Fatalf("activate: %v", err)
		}
		sb.assertNoLedgers()
	})
}

func TestStoreDirectoriesArePrivate(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("d/f", "ORIGINAL")
		sb.moduleFile("p", "d/f", "p")
		must(t, sb.activate("p"))
		for _, rel := range []string{"backups", "backups/orig", "backups/orig/etc/d"} {
			info, err := os.Stat(filepath.Join(sb.s.StateDir, rel))
			must(t, err)
			if info.Mode().Perm() != 0o700 {
				t.Errorf("%s is %v, want 0700", rel, info.Mode().Perm())
			}
		}
	})
}
