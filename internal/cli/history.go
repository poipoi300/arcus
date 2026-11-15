package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/grunclepug/arcus/internal/history"
	"github.com/grunclepug/arcus/internal/host"
)

// cmdHistory lists the last changes to a state directory. The system half's
// repository is readable by root only (it holds copies of root's files), so
// its log is read by the root side.
func (a *app) cmdHistory(args []string) error {
	var o opts
	fs := a.flags("history", &o, false, false)
	n := fs.Int("n", 20, "how many changes to list")
	system := fs.Bool("system", false, "list the system half's changes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *n < 1 {
		return errors.New("usage: arcus history [-n N] [-system]")
	}
	if err := a.load(&o); err != nil {
		return err
	}
	if *system {
		return a.sudo([]string{"-history", "-n", strconv.Itoa(*n), "-home", a.paths.Home, "-config", a.paths.Config})
	}
	return a.printHistory(a.userHalf().dir, *n)
}

// printHistory prints the last n changes of the state directory dir.
func (a *app) printHistory(dir string, n int) error {
	out, err := history.Log(dir, n)
	if errors.Is(err, history.ErrNoRepo) {
		fmt.Fprintf(a.stdout, "No history in %s yet; it starts with the next command that changes state.\n", dir)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprint(a.stdout, out)
	return nil
}

// cmdRollback puts the host file back as it was at a revision of the user
// half's history and then does what apply does: the same plan, confirmation
// and flags. Declined or failed, the host file stays as it is.
func (a *app) cmdRollback(args []string) error {
	var o opts
	fs := a.flags("rollback", &o, true, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: arcus rollback [-f] [-y] [-n] <rev>")
	}
	rev := fs.Arg(0)
	a.cmdline = "rollback " + rev
	if err := a.load(&o); err != nil {
		return err
	}

	// change saves the host file only after the steps ran, so declining the
	// plan, or a failure, leaves the current one untouched. The edit runs
	// under the user lock, so the revision is read, and what is pending
	// recorded, with no other arcus changing the state.
	return a.change(&o, true, func(f *host.File) error {
		data, err := history.Show(a.userHalf().dir, rev, filepath.Base(f.Path))
		if err != nil {
			return err
		}
		old, err := host.Parse(f.Path, data)
		if err != nil {
			return fmt.Errorf("the host file at %s: %w", rev, err)
		}
		// Edits made by hand since the last command are recoverable.
		if !o.dry {
			if err := history.Commit(a.userHalf().dir, "before rollback "+rev); err != nil {
				fmt.Fprintf(a.stderr, "warning: recording the state history: %v\n", err)
			}
		}
		a.printHostChanges(f, old, rev)
		f.Profiles, f.Modules = old.Profiles, old.Modules
		return nil
	})
}

// printHostChanges shows what putting the host file's lists back as of rev
// changes: entries it would drop ("-") and bring back ("+").
func (a *app) printHostChanges(now, old *host.File, rev string) {
	var lines []string
	for _, l := range []struct {
		kind     string
		now, old []string
	}{{"profile", now.Profiles, old.Profiles}, {"module", now.Modules, old.Modules}} {
		before := len(lines)
		for _, name := range l.now {
			if !slices.Contains(l.old, name) {
				lines = append(lines, fmt.Sprintf("- %s %s", l.kind, name))
			}
		}
		for _, name := range l.old {
			if !slices.Contains(l.now, name) {
				lines = append(lines, fmt.Sprintf("+ %s %s", l.kind, name))
			}
		}
		if len(lines) == before && !slices.Equal(l.now, l.old) {
			lines = append(lines, fmt.Sprintf("~ %ss are listed in another order", l.kind))
		}
	}
	if len(lines) == 0 {
		fmt.Fprintf(a.stdout, "The host file already lists what it did at %s.\n", rev)
		return
	}
	fmt.Fprintf(a.stdout, "Host file %s, as of %s:\n", now.Path, rev)
	for _, line := range lines {
		fmt.Fprintf(a.stdout, "        %s\n", line)
	}
}

// doctorHistory notes that nothing is recorded without git.
func (a *app) doctorHistory(r *report) {
	if history.Available() {
		return
	}
	fmt.Fprintln(r.w, "History:")
	fmt.Fprintf(r.w, "  · %v\n", history.ErrNoGit)
}
