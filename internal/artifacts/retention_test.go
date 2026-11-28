package artifacts

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAged(t *testing.T, path string, age time.Duration, now time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	mtime := now.Add(-age)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	t.Fatalf("lstat %s: %v", path, err)
	return false
}

func TestSweepRemovesOnlyOldFilesAndEmptyDirs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := Retention + 24*time.Hour
	young := time.Hour

	oldOne := filepath.Join(dir, "manifests", "1", "svc", "kind", "old.bin")
	youngOne := filepath.Join(dir, "manifests", "1", "svc", "kind", "young.bin")
	oldOnlyDir := filepath.Join(dir, "manifests", "2", "svc", "kind", "old.bin")
	youngOther := filepath.Join(dir, "manifests", "3", "svc", "kind", "young.bin")
	crashed := filepath.Join(dir, ".tmp-crashed")
	writeAged(t, oldOne, old, now)
	writeAged(t, youngOne, young, now)
	writeAged(t, oldOnlyDir, old, now)
	writeAged(t, youngOther, young, now)
	writeAged(t, crashed, old, now)

	removed, err := Sweep(context.Background(), dir, Retention, now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}
	for _, gone := range []string{oldOne, oldOnlyDir, crashed, filepath.Join(dir, "manifests", "2")} {
		if exists(t, gone) {
			t.Errorf("%s still exists", gone)
		}
	}
	for _, kept := range []string{youngOne, youngOther, filepath.Join(dir, "manifests", "1"), filepath.Join(dir, "manifests", "3"), dir} {
		if !exists(t, kept) {
			t.Errorf("%s was removed", kept)
		}
	}

	if _, err := Sweep(context.Background(), dir, Retention, now.Add(60*24*time.Hour)); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if !exists(t, dir) {
		t.Error("the store directory itself was removed when it became empty")
	}
}

func TestSweepMissingDirIsNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	removed, err := Sweep(context.Background(), missing, Retention, time.Now())
	if err != nil || removed != 0 {
		t.Errorf("Sweep of a missing dir = %d, %v; want 0, nil", removed, err)
	}
}

func TestSweepLeavesSymlinkTargets(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	now := time.Now()
	target := filepath.Join(outside, "old.bin")
	writeAged(t, target, Retention+24*time.Hour, now)
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := Sweep(context.Background(), dir, Retention, now); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !exists(t, target) {
		t.Error("the file a symlink points to outside the store was removed")
	}
}
