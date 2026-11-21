package layer

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Build the on-disk state that the fork left behind without calling Activate
// or using the port's store path helpers.
func TestForkStoreLedgerRestoresAndAuditsClean(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		const rel = "config/file"
		host := sb.hostFile(rel, "deployed")
		src := sb.moduleFile("config", rel, "deployed")
		backup := filepath.Join(sb.s.StateDir, "backups", "orig", "etc", rel)
		must(t, os.MkdirAll(filepath.Dir(backup), 0o700))
		must(t, os.WriteFile(backup, []byte("original"), 0o600))

		fields := `"kind":"link"`
		if sb.s.Copy {
			must(t, os.Chmod(host, 0o644))
			fields = fmt.Sprintf(`"kind":"copy","sha256":%q,"mode":"0644","uid":%d,"gid":%d`,
				hashBytes([]byte("deployed")), os.Geteuid(), os.Getegid())
		} else {
			must(t, os.Remove(host))
			must(t, os.Symlink(src, host))
		}
		ledger := fmt.Sprintf(`{"version":1,"module":"config","status":"active","entries":[{"host":%q,"src":%q,"backup":%q,%s}]}`,
			host, src, backup, fields)
		must(t, os.MkdirAll(filepath.Join(sb.s.StateDir, "ledger"), 0o755))
		must(t, os.WriteFile(filepath.Join(sb.s.StateDir, "ledger", "config.json"), []byte(ledger), 0o644))
		sb.stack = []string{"config"}

		findings, err := sb.s.Audit("config", sb.stack)
		must(t, err)
		if len(findings) != 0 {
			t.Errorf("fork deployment audit: %v", findings)
		}

		stray := backup + ".1"
		must(t, os.WriteFile(stray, []byte("unreferenced"), 0o600))
		dangling, err := sb.s.DanglingBackups()
		must(t, err)
		if !slices.Equal(dangling, []string{stray}) {
			t.Errorf("dangling backups = %v, want only %s", dangling, stray)
		}
		must(t, os.Remove(stray))

		// The host's parent was removed with the deployment.
		must(t, os.Remove(host))
		must(t, os.Remove(filepath.Dir(host)))
		must(t, sb.deactivate("config", false))
		if got := sb.read(rel); got != "original" {
			t.Errorf("restored file = %q", got)
		}
		sb.assertNoLedgers()
		dangling, err = sb.s.DanglingBackups()
		must(t, err)
		if len(dangling) != 0 || exists(backup) {
			t.Errorf("backup store after restore: dangling = %v, backup exists = %t", dangling, exists(backup))
		}
	})
}
