// Package history keeps a state directory under git, so the machine's arcus
// state (the host file, state, ledgers and the files arcus displaced) has a
// history. It runs the git binary; without one, recording is a silent no-op.
package history

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/grunclepug/arcus/internal/atomicfile"
)

// ErrNoRepo is returned when a directory has no history yet.
var ErrNoRepo = errors.New("no history recorded yet")

// ErrNoGit is returned by the commands that read history when git is missing.
var ErrNoGit = errors.New("git not found: state history is off")

// ignore lists what a state directory holds that isn't state: the lock file,
// the temp files atomicfile.Write leaves if it dies (.<name>.<random>.tmp),
// and the temp copies of a move across filesystems (<name>.arcus-tmp).
const ignore = `.lock
.*.tmp
*.arcus-tmp
`

// Available reports whether git is on PATH.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// git runs git in dir. safe.directory lets root read a user's repository and
// the user the system half's, which differ in owner; hooks never run, and
// GIT_DIR and friends from the caller's environment never redirect it.
func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "safe.directory=" + dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_DIR=") && !strings.HasPrefix(kv, "GIT_WORK_TREE=") && !strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(exit.Stderr)))
		}
		return out, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}

// Init makes dir a repository if it isn't one: ignoring the lock and temp
// files, committing as arcus whoever runs it (root or a user without a git
// identity), never signing, and readable by its owner only. The last matters
// for the system half, whose backups may be files only root may read; git
// stores their content with the default, world-readable modes.
func Init(dir string) error {
	if !Available() {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return nil
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return err
	}
	if _, err := git(dir, "init", "-q"); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(dir, ".git"), 0o700); err != nil {
		return err
	}
	if err := atomicfile.Write(filepath.Join(dir, ".gitignore"), []byte(ignore), 0o644); err != nil {
		return err
	}
	for _, kv := range [][2]string{{"user.name", "arcus"}, {"user.email", "arcus@localhost"}, {"commit.gpgsign", "false"}} {
		if _, err := git(dir, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// Commit records whatever changed in dir since the last commit under
// message. It does nothing if dir doesn't exist, nothing changed or git is
// missing. A failure never undoes the arcus command it follows: the caller
// reports it and carries on.
func Commit(dir, message string) error {
	if !Available() {
		return nil
	}
	if strings.TrimSpace(message) == "" {
		message = "arcus"
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil
	}
	if err := Init(dir); err != nil {
		return err
	}
	if _, err := git(dir, "add", "-A"); err != nil {
		return err
	}
	// Exit status 1 means there is something staged.
	_, err := git(dir, "diff", "--cached", "--quiet")
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return err
	}
	_, err = git(dir, "commit", "-q", "--no-verify", "-m", message)
	return err
}

// Log returns the last n commits of dir, newest first, one per line:
// abbreviated hash, date and message.
func Log(dir string, n int) (string, error) {
	if !Available() {
		return "", ErrNoGit
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", ErrNoRepo
	}
	out, err := git(dir, "log", "--oneline", "--date=short", "--format=%h %ad %s", fmt.Sprintf("-n%d", n))
	if err != nil {
		if _, headErr := git(dir, "rev-parse", "--verify", "-q", "HEAD"); headErr != nil {
			return "", ErrNoRepo
		}
		return "", err
	}
	return string(out), nil
}

// Show returns the content of path as it was at rev.
func Show(dir, rev, path string) ([]byte, error) {
	if !Available() {
		return nil, ErrNoGit
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return nil, ErrNoRepo
	}
	// A rev that starts with "-" would be read as an option.
	if strings.HasPrefix(rev, "-") {
		return nil, fmt.Errorf("%q is not a revision of the history", rev)
	}
	if _, err := git(dir, "rev-parse", "--verify", "-q", rev+"^{commit}"); err != nil {
		return nil, fmt.Errorf("%q is not a revision of the history", rev)
	}
	out, err := git(dir, "show", rev+":"+path)
	if err != nil {
		return nil, fmt.Errorf("%s does not exist at %s: %w", path, rev, err)
	}
	return out, nil
}
