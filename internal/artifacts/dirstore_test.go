package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcss-labs/yamdrake/internal/secrets"
)

func testCipher(t *testing.T) secrets.Cipher {
	t.Helper()
	c, err := secrets.NewLocal([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return c
}

func newTestStore(t *testing.T) (Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewDir(dir, testCipher(t))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	return s, dir
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestDirPutGetRoundTripAndChecksum(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	data := []byte("services:\n  api: {}\n")

	key, sum, err := s.Put(ctx, 7, "api", "manifest", "app.yaml", false, data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := "manifests/7/api/manifest/app.yaml"; key != want {
		t.Errorf("key = %q, want %q", key, want)
	}
	if want := sha256Hex(data); sum != want {
		t.Errorf("sha256 = %q, want %q", sum, want)
	}
	got, err := s.Get(ctx, key, sum)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Get returned %q, want %q", got, data)
	}
}

func TestDirPreviousCopiesUseSeparateKeys(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	currentKey, currentSum, err := s.Put(ctx, 7, "api", "manifest", "app.yaml", false, []byte("current"))
	if err != nil {
		t.Fatalf("Put current: %v", err)
	}
	previousKey, previousSum, err := s.Put(ctx, 7, "api", "manifest", "app.yaml", true, []byte("previous"))
	if err != nil {
		t.Fatalf("Put previous: %v", err)
	}
	if currentKey == previousKey {
		t.Fatalf("current and previous share key %q", currentKey)
	}
	if want := "manifests/7/api/previous/manifest/app.yaml"; previousKey != want {
		t.Errorf("previous key = %q, want %q", previousKey, want)
	}
	if got, err := s.Get(ctx, currentKey, currentSum); err != nil || string(got) != "current" {
		t.Errorf("current Get = %q, %v", got, err)
	}
	if got, err := s.Get(ctx, previousKey, previousSum); err != nil || string(got) != "previous" {
		t.Errorf("previous Get = %q, %v", got, err)
	}
}

func TestDirBytesOnDiskAreNotPlaintext(t *testing.T) {
	s, dir := newTestStore(t)
	marker := []byte("plaintext-marker-7f3a")

	key, _, err := s.Put(context.Background(), 1, "svc", "kind", "name", false, marker)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key))) //nolint:gosec // the test reads its own temp dir
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	if bytes.Contains(raw, marker) {
		t.Fatal("stored file contains the plaintext")
	}
}

func TestDirCorruptFileIsErrCorrupt(t *testing.T) {
	s, dir := newTestStore(t)
	ctx := context.Background()

	key, sum, err := s.Put(ctx, 1, "svc", "kind", "name", false, []byte("config body"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := filepath.Join(dir, filepath.FromSlash(key))
	raw, err := os.ReadFile(path) //nolint:gosec // the test reads its own temp dir
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	raw[len(raw)-1] ^= 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil { //nolint:gosec // the test writes its own temp dir

		t.Fatalf("corrupt stored file: %v", err)
	}
	if _, err := s.Get(ctx, key, sum); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Get of a flipped byte: err = %v, want ErrCorrupt", err)
	}

	goodKey, _, err := s.Put(ctx, 2, "svc", "kind", "name", false, []byte("another body"))
	if err != nil {
		t.Fatalf("Put second: %v", err)
	}
	if _, err := s.Get(ctx, goodKey, sha256Hex([]byte("not the body"))); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Get with a wrong checksum: err = %v, want ErrCorrupt", err)
	}
}

func TestDirRefusesKeysOutsideItsDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "store")
	s, err := NewDir(dir, testCipher(t))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	cipher := testCipher(t)
	keys := []string{
		"manifests/../x",
		"../manifests/1/a/b/c",
		"/etc/passwd",
		"manifests/1/a/./b",
		"other/1/a/b/c",
		"manifests\\..\\x",
	}
	for _, key := range keys {
		// Plant a real, decryptable object where an unchecked join would read it,
		// with its true checksum. Only the key checks can stop Get from returning it.
		data := []byte("planted for " + key)
		target := filepath.Join(dir, filepath.FromSlash(key))
		sealed, err := cipher.Encrypt(context.Background(), artifactPurpose, data)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatalf("mkdir for %q: %v", key, err)
		}
		if err := os.WriteFile(target, sealed, 0o600); err != nil {
			t.Fatalf("plant %q: %v", key, err)
		}

		got, err := s.Get(context.Background(), key, sha256Hex(data))
		if err == nil || !strings.Contains(err.Error(), "is outside") {
			t.Errorf("Get(%q) err = %v, want a refusal containing \"is outside\"", key, err)
		}
		if got != nil {
			t.Errorf("Get(%q) returned data %q", key, got)
		}
	}
}

func TestDirPutRejectsBadComponents(t *testing.T) {
	s, _ := newTestStore(t)
	bad := []string{"", "a/b", ".", "a..b", "a\x00b", "a\\b"}
	for _, v := range bad {
		if _, _, err := s.Put(context.Background(), 1, v, "kind", "name", false, []byte("x")); err == nil {
			t.Errorf("service %q accepted", v)
		}
		if _, _, err := s.Put(context.Background(), 1, "svc", v, "name", false, []byte("x")); err == nil {
			t.Errorf("kind %q accepted", v)
		}
		if _, _, err := s.Put(context.Background(), 1, "svc", "kind", v, false, []byte("x")); err == nil {
			t.Errorf("name %q accepted", v)
		}
	}
}

func TestDirInterruptedWriteLeavesNoFileUnderFinalName(t *testing.T) {
	s, dir := newTestStore(t)
	s.(*dirStore).beforeRename = func(string) error { return errors.New("simulated crash") }

	key, _, err := s.Put(context.Background(), 1, "svc", "kind", "name", false, []byte("body"))
	if err == nil {
		t.Fatal("Put succeeded although the write was interrupted")
	}
	if _, statErr := os.Stat(filepath.Join(dir, filepath.FromSlash("manifests/1/svc/kind/name"))); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("final file exists after an interrupted write (key %q): %v", key, statErr)
	}
	leftover := false
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), ".tmp-") {
			leftover = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if leftover {
		t.Error("a .tmp- file was left behind")
	}
}

func TestDirFilesAreOwnerOnly(t *testing.T) {
	s, dir := newTestStore(t)
	key, _, err := s.Put(context.Background(), 1, "svc", "kind", "name", false, []byte("body"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
}

func TestDirPutRefusesDataOverTheLimit(t *testing.T) {
	s, _ := newTestStore(t)
	data := make([]byte, MaxObjectBytes+1)
	if _, _, err := s.Put(context.Background(), 1, "svc", "kind", "name", false, data); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Put over the limit: err = %v, want ErrTooLarge", err)
	}
}

func TestDirPutRecreatesDirectoryRemovedBySweep(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	fired := false
	s.(*dirStore).beforeCreateTemp = func(parent string) {
		if !fired {
			fired = true
			// Stands in for the sweep removing the just-created, still empty directory.
			if err := os.Remove(parent); err != nil {
				t.Fatalf("remove %s: %v", parent, err)
			}
		}
	}

	data := []byte("body after a sweep")
	key, sum, err := s.Put(ctx, 9, "svc", "kind", "name", false, data)
	if err != nil {
		t.Fatalf("Put after the sweep removed its directory: %v", err)
	}
	if !fired {
		t.Fatal("the hook did not run, so the race was not reproduced")
	}
	got, err := s.Get(ctx, key, sum)
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("Get = %q, %v; want the stored body", got, err)
	}
}

func TestAFileNamedPreviousDoesNotCollideWithPreviousCopies(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	keyOfFile, sumOfFile, err := s.Put(ctx, 7, "api", "manifest", "previous", false, []byte("a file called previous"))
	if err != nil {
		t.Fatalf("Put current file named previous: %v", err)
	}
	keyOfCopy, sumOfCopy, err := s.Put(ctx, 7, "api", "manifest", "app.yaml", true, []byte("a previous copy"))
	if err != nil {
		t.Fatalf("Put previous copy: %v", err)
	}
	keyOfCopyOfFile, sumOfCopyOfFile, err := s.Put(ctx, 7, "api", "manifest", "previous", true, []byte("a previous copy of previous"))
	if err != nil {
		t.Fatalf("Put previous copy of a file named previous: %v", err)
	}
	for key, want := range map[string][2]string{
		keyOfFile:       {sumOfFile, "a file called previous"},
		keyOfCopy:       {sumOfCopy, "a previous copy"},
		keyOfCopyOfFile: {sumOfCopyOfFile, "a previous copy of previous"},
	} {
		if got, err := s.Get(ctx, key, want[0]); err != nil || string(got) != want[1] {
			t.Errorf("Get %s = %q, %v; want %q", key, got, err, want[1])
		}
	}

	// A kind called previous would sit where the previous copies are kept.
	if _, _, err := s.Put(ctx, 7, "api", "previous", "app.yaml", false, []byte("x")); err == nil {
		t.Error("a kind named previous was accepted")
	}
}
