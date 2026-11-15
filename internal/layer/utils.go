package layer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/grunclepug/arcus/internal/atomicfile"
)

// access(2) mode bits; the syscall package does not export them on Linux.
const (
	accessW = 0x2
	accessX = 0x1
)

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// isWithin reports whether path is root or lies beneath it.
func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// VerifyWritePrivilege checks that the nearest existing ancestor directory of
// path is a directory this process can create entries in. Unlike checking the
// immediate parent, this accepts targets whose parent directories do not exist
// yet and will be created during activation.
func VerifyWritePrivilege(path string) error {
	dir := filepath.Dir(path)
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("cannot place %q: ancestor %q is not a directory", path, dir)
			}
			if err := syscall.Access(dir, accessW|accessX); err != nil {
				return fmt.Errorf("insufficient privilege to write into %q: %w", dir, err)
			}
			return nil
		}
		if !errors.Is(err, iofs.ErrNotExist) {
			return fmt.Errorf("inspecting %q: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return err
		}
		dir = parent
	}
}

// missingDirs returns the ancestors of dir (inclusive) that do not exist yet,
// shallowest first, i.e. the directories os.MkdirAll(dir) would create.
func missingDirs(dir string) []string {
	var missing []string
	for !exists(dir) {
		missing = append([]string{dir}, missing...)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return missing
}

// uniquePath returns base if it is free, otherwise base.1, base.2, ...
func uniquePath(base string) string {
	if !exists(base) {
		return base
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s.%d", base, i)
		if !exists(candidate) {
			return candidate
		}
	}
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %q: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readRegular reads a module file that is about to be copied as root. It
// refuses symlinks, so a user-writable module can't make root copy a file
// the user could not read.
func readRegular(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return io.ReadAll(f)
}

// copyMode is the mode a system file is deployed with: 0755 if the module
// file is executable by anyone, 0644 otherwise. It never depends on the
// user's umask and never carries write bits for group or others.
func copyMode(src os.FileMode) string {
	if src&0o111 != 0 {
		return "0755"
	}
	return "0644"
}

func parseMode(mode string) (os.FileMode, error) {
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid mode %q: %w", mode, err)
	}
	return os.FileMode(m), nil
}

// owns reports whether loc currently holds exactly the deployment described by e.
func owns(e Entry, loc string) bool {
	info, err := os.Lstat(loc)
	if err != nil {
		return false
	}
	switch e.Kind {
	case KindLink:
		if info.Mode()&os.ModeSymlink == 0 {
			return false
		}
		target, err := os.Readlink(loc)
		return err == nil && target == e.Src
	case KindCopy:
		if !info.Mode().IsRegular() {
			return false
		}
		sum, err := hashFile(loc)
		return err == nil && sum == e.SHA256
	}
	return false
}

// renameDurable renames and fsyncs both parent directories.
func renameDurable(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	if err := atomicfile.SyncDir(filepath.Dir(to)); err != nil {
		return err
	}
	if filepath.Dir(from) != filepath.Dir(to) {
		return atomicfile.SyncDir(filepath.Dir(from))
	}
	return nil
}

func removeDurable(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(path))
}

// symlinkReplace points link at target, atomically replacing anything at link.
func symlinkReplace(target, link string) error {
	tmp := uniquePath(link + ".arcus-tmp")
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := renameDurable(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// moveDurable moves a file or symlink and fsyncs the directories involved.
// Backups and moved-aside files live in the state directory, which can be on
// another filesystem than the host path (/etc and a separate /var), where
// rename fails with EXDEV; then the file is copied and the original removed.
//
// A crash can leave both copies, never neither: the destination only appears
// by an atomic rename of a fully written, fsynced temp file, and the
// original is removed only after that rename and its directory fsync. With
// both there, no step loses data:
//   - activation: planStep finds the host's file where a deployment should be
//     and, unless that is byte-identical to a copy deployment (which is
//     removed, the identical backup restored), calls it foreign. Deactivate
//     refuses without -f, Rollback leaves it in place, and doctor reports it.
//   - restoring a backup or moving a file aside: the entry is still in the
//     ledger and the source still exists, so the next run sees the same
//     situation, and the duplicate at the destination shows up as a foreign
//     file (-f moves it aside) or is one more file in foreign/.
//
// A temp file left by a crash ends in ".arcus-tmp" and, in the backup store,
// shows up in doctor as a backup no ledger references.
func moveDurable(from, to string) error {
	err := renameDurable(from, to)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyDurable(from, to); err != nil {
		return fmt.Errorf("copying %q across filesystems: %w", from, err)
	}
	return removeDurable(from)
}

// copyDurable writes a copy of from at to, keeping a regular file's content,
// mode, owner and mtime or recreating a symlink, and fsyncs it. Anything else
// is refused. to only ever appears complete.
func copyDurable(from, to string) error {
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	tmp := uniquePath(to + ".arcus-tmp")
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		err = copyLink(from, tmp, info)
	case info.Mode().IsRegular():
		err = copyFile(from, tmp, info)
	default:
		err = fmt.Errorf("%s is neither a regular file nor a symlink", from)
	}
	if err == nil {
		err = os.Rename(tmp, to)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(to))
}

func copyLink(from, tmp string, info os.FileInfo) error {
	target, err := os.Readlink(from)
	if err != nil {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return os.Lchown(tmp, int(st.Uid), int(st.Gid))
	}
	return nil
}

func copyFile(from, tmp string, info os.FileInfo) error {
	src, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	// Owner before mode: chown clears setuid and setgid.
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := dst.Chown(int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	if err := dst.Chmod(info.Mode()); err != nil {
		return err
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	return dst.Close()
}
