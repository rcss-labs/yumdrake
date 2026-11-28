package logs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcss-labs/yamdrake/internal/secrets"
)

func archiveTestCipher(t *testing.T, key string) secrets.Cipher {
	t.Helper()
	c, err := secrets.NewLocal([]byte(key))
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return c
}

func TestDirArchiveEncryptsAtRest(t *testing.T) {
	dir := t.TempDir()
	a, err := NewDirArchive(dir, archiveTestCipher(t, "0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewDirArchive: %v", err)
	}
	ctx := context.Background()
	data, err := encodeArchive([]archivedLine{{RunID: 1, LegID: 2, Seq: 1, Level: "info", Text: "plaintext-marker-91c2"}})
	if err != nil {
		t.Fatalf("encodeArchive: %v", err)
	}
	key := archiveKey(1, 2)
	if err := a.Put(ctx, key, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key))) //nolint:gosec // the test reads its own temp dir
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	if bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		t.Error("stored file starts with the gzip magic; it is not encrypted")
	}
	if bytes.Contains(raw, []byte("plaintext-marker-91c2")) {
		t.Error("stored file contains a plaintext line")
	}

	got, err := a.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("Get did not return the original bytes")
	}
}

func TestDirArchiveRejectsOtherKey(t *testing.T) {
	dir := t.TempDir()
	mine, err := NewDirArchive(dir, archiveTestCipher(t, "0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewDirArchive mine: %v", err)
	}
	other, err := NewDirArchive(dir, archiveTestCipher(t, "ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatalf("NewDirArchive other: %v", err)
	}
	ctx := context.Background()
	key := archiveKey(3, 4)
	if err := mine.Put(ctx, key, []byte("lines")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, err := other.Get(ctx, key); err == nil {
		t.Errorf("a different key read the archive: %q", got)
	}
}

func TestDirArchiveFilesAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	a, err := NewDirArchive(dir, archiveTestCipher(t, "0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewDirArchive: %v", err)
	}
	key := archiveKey(5, 6)
	if err := a.Put(context.Background(), key, []byte("lines")); err != nil {
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

func TestASwappedArchiveFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	a, err := NewDirArchive(dir, archiveTestCipher(t, "0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewDirArchive: %v", err)
	}
	ctx := context.Background()
	keyA, keyB := archiveKey(1, 1), archiveKey(1, 2)
	if err := a.Put(ctx, keyA, []byte("lines of leg one")); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	if err := a.Put(ctx, keyB, []byte("lines of leg two")); err != nil {
		t.Fatalf("Put B: %v", err)
	}
	pathA := filepath.Join(dir, filepath.FromSlash(keyA))
	pathB := filepath.Join(dir, filepath.FromSlash(keyB))
	rawA, err := os.ReadFile(pathA) //nolint:gosec // the test reads its own temp dir
	if err != nil {
		t.Fatalf("read A: %v", err)
	}
	if err := os.WriteFile(pathB, rawA, 0o600); err != nil { //nolint:gosec // the test writes its own temp dir
		t.Fatalf("swap: %v", err)
	}
	got, err := a.Get(ctx, keyB)
	if err == nil {
		t.Fatalf("a swapped archive file was read: %q", got)
	}
	if !strings.Contains(err.Error(), keyB) || !strings.Contains(err.Error(), "cannot decrypt") {
		t.Errorf("error = %v, want one that says %s cannot be decrypted", err, keyB)
	}
}
