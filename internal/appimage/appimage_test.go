package appimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/state"
)

func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "<missing>"
	}
	return string(data)
}

// Every combination of record, file and self_update.
func TestDecide(t *testing.T) {
	pinned, old := digest("v2"), digest("v1")
	rec := func(sum string, selfUpdate bool) *state.AppImage {
		return &state.AppImage{SHA256: sum, SelfUpdate: selfUpdate}
	}
	tests := []struct {
		name       string
		file       string // "" for none
		rec        *state.AppImage
		selfUpdate bool
		want       Action
	}{
		{"missing, unrecorded", "", nil, false, Install},
		{"missing, recorded", "", rec(pinned, false), false, Install},
		{"missing, recorded, self-updating", "", rec(pinned, true), true, Install},
		{"pinned, unrecorded", "v2", nil, false, Adopt},
		{"pinned, unrecorded, self-updating", "v2", nil, true, Adopt},
		{"other, unrecorded", "other", nil, false, Replace},
		{"other, unrecorded, self-updating: as it is", "other", nil, true, Adopt},
		{"pinned, recorded", "v2", rec(pinned, false), false, Keep},
		{"pin moved, file unchanged", "v1", rec(old, false), false, Update},
		{"pin moved, file modified", "other", rec(old, false), false, Replace},
		{"pinned but modified", "other", rec(pinned, false), false, Replace},
		{"pin moved, file already the new one", "v2", rec(old, false), false, Record},
		{"self-updating, changed by the app", "other", rec(old, true), true, Keep},
		{"self-updating, pin moved", "v1", rec(old, true), true, Keep},
		{"self_update turned on", "other", rec(old, false), true, Record},
		{"self_update turned off, pinned", "v2", rec(old, true), false, Record},
		{"self_update turned off, unchanged", "v1", rec(old, true), false, Update},
		{"self_update turned off, the app updated it", "other", rec(old, true), false, Replace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "Applications", "a.AppImage")
			if tt.file != "" {
				write(t, path, tt.file)
			}
			d := module.AppImage{URL: "https://x.org/a", SHA256: pinned, Path: "Applications/a.AppImage", SelfUpdate: tt.selfUpdate}
			got, f, err := Decide(path, d, tt.rec)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			if tt.file != "" && f.SHA256 != "" && f.SHA256 != digest(tt.file) {
				t.Errorf("sha256 = %s", f.SHA256)
			}
			if tt.rec != nil && tt.selfUpdate && f.SHA256 != "" {
				t.Error("a recorded self-updating AppImage must not be read")
			}
		})
	}
}

func TestDecideRefusesNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.AppImage")
	write(t, filepath.Join(dir, "real"), "v2")
	if err := os.Symlink(filepath.Join(dir, "real"), path); err != nil {
		t.Fatal(err)
	}
	d := module.AppImage{SHA256: digest("v2")}
	if _, _, err := Decide(path, d, nil); err == nil || !strings.Contains(err.Error(), "isn't a regular file") {
		t.Errorf("symlink: %v", err)
	}
}

// Sum trusts the record while the stamp matches and reads the file once
// anything about it changed.
func TestSumCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a")
	write(t, path, "v1")
	f, err := Sum(path, nil)
	if err != nil || f.SHA256 != digest("v1") || f.Stamp.Inode == 0 || f.Stamp.Size != 2 {
		t.Fatalf("Sum = %+v, %v", f, err)
	}
	// A record with this stamp is believed, even when it is wrong; that is
	// what saves reading a large file.
	cached := &state.AppImage{SHA256: "cached", Stamp: f.Stamp}
	if got, _ := Sum(path, cached); got.SHA256 != "cached" {
		t.Errorf("cache not used: %s", got.SHA256)
	}
	// Same size, new content and mtime: read again.
	write(t, path, "v2")
	if err := os.Chtimes(path, time.Time{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := Sum(path, cached); got.SHA256 != digest("v2") {
		t.Errorf("stale cache used: %s", got.SHA256)
	}
}

func TestRemovable(t *testing.T) {
	home := t.TempDir()
	st := &state.State{}
	add := func(p, content string, selfUpdate bool) {
		write(t, filepath.Join(home, p), content)
		f, err := Sum(filepath.Join(home, p), nil)
		if err != nil {
			t.Fatal(err)
		}
		st.SetAppImage("m", p, state.AppImage{SHA256: f.SHA256, SelfUpdate: selfUpdate, Stamp: f.Stamp})
	}
	add("unchanged", "v1", false)
	add("modified", "v1", false)
	add("updated-by-app", "v1", true)
	add("gone", "v1", false)
	add("dir", "v1", true)
	write(t, filepath.Join(home, "modified"), "mine")
	write(t, filepath.Join(home, "updated-by-app"), "v2")
	for _, p := range []string{"gone", "dir"} {
		if err := os.Remove(filepath.Join(home, p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(home, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Removable(home, st)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"unchanged": true, "modified": false, "updated-by-app": true}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// serve answers every request with body.
func serve(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// entries lists a directory, so a test sees leftover temp files.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestFetch(t *testing.T) {
	srv := serve(t, "#!/bin/sh\nexit 1\n")
	home := t.TempDir()
	path := filepath.Join(home, "Applications", "sub", "a.AppImage")
	stamp, err := Fetch(context.Background(), srv.Client(), srv.URL+"/a", digest("#!/bin/sh\nexit 1\n"), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode() != 0o755 {
		t.Fatalf("installed file: %v, %v", info, err)
	}
	if read(t, path) != "#!/bin/sh\nexit 1\n" {
		t.Errorf("content = %q", read(t, path))
	}
	if got := entries(t, filepath.Dir(path)); len(got) != 1 {
		t.Errorf("leftovers next to the file: %v", got)
	}
	if f, err := Sum(path, nil); err != nil || f.Stamp != stamp {
		t.Errorf("returned stamp %+v, file has %+v (%v)", stamp, f.Stamp, err)
	}
}

// A replacement only happens once the download is verified: aside sees
// the old file in place and nothing half-written, and the new file
// replaces it in one rename.
func TestFetchReplaces(t *testing.T) {
	srv := serve(t, "v2")
	path := filepath.Join(t.TempDir(), "a.AppImage")
	write(t, path, "v1")
	moved := filepath.Join(t.TempDir(), "aside")
	aside := func() error {
		if read(t, path) != "v1" {
			t.Errorf("the old file changed before aside: %q", read(t, path))
		}
		return os.Rename(path, moved)
	}
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, aside); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != "v2" || read(t, moved) != "v1" {
		t.Errorf("file = %q, set aside = %q", read(t, path), read(t, moved))
	}
}

// A wrong digest, a failed request or a cancelled one leave everything as
// it was: no temp file, no new directory, the old file untouched, aside
// never called.
func TestFetchFailsCleanly(t *testing.T) {
	ok := serve(t, "tampered")
	missing := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(missing.Close)
	offline := httptest.NewTLSServer(http.NotFoundHandler())
	offlineURL, offlineClient := offline.URL, offline.Client()
	offline.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		client  *http.Client
		url     string
		wantErr []string
	}{
		{"digest mismatch", context.Background(), ok.Client(), ok.URL + "/a.AppImage",
			[]string{ok.URL + "/a.AppImage", "expected " + digest("v2"), "got " + digest("tampered"), "nothing was installed"}},
		{"not found", context.Background(), missing.Client(), missing.URL, []string{"404", "nothing was installed"}},
		{"offline", context.Background(), offlineClient, offlineURL, []string{"downloading " + offlineURL, "nothing was installed"}},
		{"cancelled", cancelled, ok.Client(), ok.URL, []string{"context canceled"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			write(t, filepath.Join(home, "old.AppImage"), "v1")
			for _, path := range []string{filepath.Join(home, "old.AppImage"), filepath.Join(home, "New", "Dir", "a.AppImage")} {
				aside := func() error { t.Error("aside ran"); return nil }
				_, err := Fetch(tt.ctx, tt.client, tt.url, digest("v2"), path, aside)
				for _, want := range tt.wantErr {
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("error %v lacks %q", err, want)
					}
				}
			}
			if got := entries(t, home); len(got) != 1 || got[0] != "old.AppImage" {
				t.Errorf("home holds %v", got)
			}
			if read(t, filepath.Join(home, "old.AppImage")) != "v1" {
				t.Error("the old file changed")
			}
		})
	}
}

func TestFetchStalls(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("v"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	defer func(d time.Duration) { StallTimeout = d }(StallTimeout)
	StallTimeout = 100 * time.Millisecond

	path := filepath.Join(t.TempDir(), "a.AppImage")
	_, err := Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, nil)
	if err == nil || !strings.Contains(err.Error(), "nothing arrived for 100ms") {
		t.Fatalf("stalled download: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a stalled download left a file")
	}
}
