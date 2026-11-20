// Package appimage inspects and downloads the vendor AppImages that modules
// declare in $HOME.
package appimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/grunclepug/arcus/internal/atomicfile"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/state"
)

// Action is what brings the file at an AppImage's path in line with its
// declaration.
type Action int

const (
	Keep    Action = iota // nothing to do
	Install               // nothing there: download it
	Update                // the file arcus installed, unchanged, but the pin moved: download over it
	Replace               // neither the pinned file nor arcus's unchanged one: set it aside, then download
	Adopt                 // not recorded, and the pinned file or (self_update) any file: record it as it is
	Record                // recorded and fine as it is, but the record is out of date
)

// File is what Decide or Sum found at a path.
type File struct {
	SHA256 string // "" when the file wasn't read
	Stamp  state.Stamp
}

// Decide returns what brings the file at path in line with d. rec is
// arcus's record of the path, nil if it has none. A self-updating AppImage
// that arcus recorded is never read: the app owns its content. Anything
// but a regular file at path is an error; arcus doesn't replace it.
func Decide(path string, d module.AppImage, rec *state.AppImage) (Action, File, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, iofs.ErrNotExist) {
		return Install, File{}, nil
	}
	if err != nil {
		return Keep, File{}, err
	}
	if !info.Mode().IsRegular() {
		return Keep, File{}, fmt.Errorf("%s is there and isn't a regular file: move it away, then apply again", path)
	}
	if rec != nil && d.SelfUpdate {
		if rec.SelfUpdate {
			return Keep, File{}, nil
		}
		return Record, File{}, nil
	}
	f, err := Sum(path, rec)
	if err != nil {
		return Keep, File{}, err
	}
	switch {
	case f.SHA256 == d.SHA256 && rec == nil:
		return Adopt, f, nil
	case f.SHA256 == d.SHA256 && (rec.SHA256 != d.SHA256 || rec.SelfUpdate):
		return Record, f, nil
	case f.SHA256 == d.SHA256:
		return Keep, f, nil
	case rec == nil && d.SelfUpdate:
		return Adopt, f, nil
	case rec != nil && f.SHA256 == rec.SHA256:
		return Update, f, nil
	}
	return Replace, f, nil
}

// Sum returns the SHA-256 and stamp of the regular file at path. While the
// file still has rec's stamp it has rec's content, which is returned
// without reading it.
func Sum(path string, rec *state.AppImage) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	stamp := stampOf(info)
	if rec != nil && rec.SHA256 != "" && rec.Stamp == stamp {
		return File{SHA256: rec.SHA256, Stamp: stamp}, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return File{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if after, err := f.Stat(); err != nil || stampOf(after) != stamp {
		return File{}, fmt.Errorf("%s changed while it was read; try again", path)
	}
	return File{SHA256: hex.EncodeToString(h.Sum(nil)), Stamp: stamp}, nil
}

// Removable reports, for every AppImage st records, whether turning it off
// may remove its file: it is still a regular file and either self-updating
// or unchanged since arcus recorded it. Paths are relative to home.
func Removable(home string, st *state.State) (map[string]bool, error) {
	removable := map[string]bool{}
	for _, paths := range st.Owned[state.HomeAppImages] {
		for _, p := range paths {
			rec := st.AppImage(p)
			path := filepath.Join(home, p)
			info, err := os.Lstat(path)
			if rec == nil || errors.Is(err, iofs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if rec.SelfUpdate {
				removable[p] = true
				continue
			}
			f, err := Sum(path, rec)
			if err != nil {
				return nil, err
			}
			removable[p] = f.SHA256 == rec.SHA256
		}
	}
	return removable, nil
}

// StallTimeout is how long a download may go without receiving anything.
var StallTimeout = time.Minute

var errStalled = errors.New("stalled")

// Fetch downloads url into a temp file next to path and checks it against
// want before anything else happens. Then it makes the file executable,
// fsyncs it, runs aside (if set; it moves the file at path out of the
// way) and renames it over path. On any failure before the rename the temp
// file and the directories Fetch created are removed, and path is as it was.
// It returns the stamp of the installed file. The download is never run.
func Fetch(ctx context.Context, client *http.Client, url, want, path string, aside func() error) (stamp state.Stamp, err error) {
	dir := filepath.Dir(path)
	created := missingDirs(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return stamp, err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		removeDirs(created)
		return stamp, err
	}
	placed := false
	defer func() {
		tmp.Close()
		if !placed {
			_ = os.Remove(tmp.Name())
			removeDirs(created)
		}
	}()

	got, err := download(ctx, client, url, tmp)
	if err != nil {
		return stamp, fmt.Errorf("downloading %s: %w; nothing was installed", url, err)
	}
	if got != want {
		return stamp, fmt.Errorf("the download of %s doesn't match the pinned sha256: expected %s, got %s; nothing was installed", url, want, got)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return stamp, err
	}
	if err := tmp.Sync(); err != nil {
		return stamp, err
	}
	for _, d := range created {
		if err := atomicfile.SyncDir(filepath.Dir(d)); err != nil {
			return stamp, err
		}
	}
	if aside != nil {
		if err := aside(); err != nil {
			return stamp, err
		}
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return stamp, fmt.Errorf("installing %s: %w", path, err)
	}
	placed = true
	if err := atomicfile.SyncDir(dir); err != nil {
		return stamp, err
	}
	info, err := tmp.Stat()
	if err != nil {
		return stamp, err
	}
	return stampOf(info), nil
}

// download writes the body of url to w and returns its SHA-256. It gives
// up when ctx ends or nothing arrives for StallTimeout.
func download(ctx context.Context, client *http.Client, url string, w io.Writer) (string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(StallTimeout, func() { cancel(errStalled) })
	defer stall.Stop()
	fail := func(err error) error {
		if errors.Is(context.Cause(ctx), errStalled) {
			return fmt.Errorf("nothing arrived for %v", StallTimeout)
		}
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	h := sha256.New()
	body := readFunc(func(p []byte) (int, error) {
		n, err := resp.Body.Read(p)
		if n > 0 {
			stall.Reset(StallTimeout)
		}
		return n, err
	})
	if _, err := io.Copy(io.MultiWriter(w, h), body); err != nil {
		return "", fail(err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// missingDirs returns dir and its ancestors that don't exist, outermost first.
func missingDirs(dir string) []string {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil || d == filepath.Dir(d) {
			break
		}
		missing = append(missing, d)
	}
	slices.Reverse(missing)
	return missing
}

// removeDirs removes the directories created, innermost first, as long as
// they are empty.
func removeDirs(created []string) {
	for _, d := range slices.Backward(created) {
		if os.Remove(d) != nil {
			return
		}
	}
}

func stampOf(info os.FileInfo) state.Stamp {
	s := state.Stamp{Size: info.Size(), MTime: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		s.CTime, s.Inode = st.Ctim.Nano(), st.Ino
	}
	return s
}
