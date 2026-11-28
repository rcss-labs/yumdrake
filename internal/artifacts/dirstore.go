package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/rcss-labs/yamdrake/internal/secrets"
)

// artifactPurpose binds every stored copy to this use in the cipher.
const artifactPurpose = "artifact"

// cipherOverhead is the room the cipher adds to a plaintext; Get allows it on
// top of MaxObjectBytes.
const cipherOverhead = 1024

type dirStore struct {
	dir    string
	cipher secrets.Cipher

	// beforeRename runs after a copy is written and synced, before it is renamed
	// into place. Tests use it to simulate a crash; it is nil in production.
	beforeRename func(tmpPath string) error

	// beforeCreateTemp runs before the temporary file is created in dir. Tests use
	// it to stand in for a sweep that removes the directory; it is nil in production.
	beforeCreateTemp func(dir string)
}

// NewDir stores encrypted copies as files under dir, which must be absolute
// (it is created when missing).
func NewDir(dir string, c secrets.Cipher) (Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("artifacts: directory %q must be absolute", dir)
	}
	if c == nil {
		return nil, errors.New("artifacts: cipher is nil")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("artifacts: create %s: %w", dir, err)
	}
	return &dirStore{dir: filepath.Clean(dir), cipher: c}, nil
}

// Put writes the encrypted copy through a temporary file and a rename, so a
// reader never sees a partial object.
func (s *dirStore) Put(ctx context.Context, runID int64, service, kind, name string, previous bool, data []byte) (string, string, error) {
	if len(data) > MaxObjectBytes {
		return "", "", fmt.Errorf("%w: %d bytes, limit %d", ErrTooLarge, len(data), MaxObjectBytes)
	}
	key, err := objectKey(runID, service, kind, name, previous)
	if err != nil {
		return "", "", err
	}
	sealed, err := s.cipher.Encrypt(ctx, artifactPurpose, data)
	if err != nil {
		return "", "", fmt.Errorf("artifacts: encrypt %s: %w", key, err)
	}
	target := filepath.Join(s.dir, filepath.FromSlash(key))
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", "", fmt.Errorf("artifacts: create %s: %w", parent, err)
	}
	if err := s.writeAtomic(parent, target, sealed); err != nil {
		return "", "", fmt.Errorf("artifacts: store %s: %w", key, err)
	}
	sum := sha256.Sum256(data)
	return key, hex.EncodeToString(sum[:]), nil
}

// createTemp creates the temporary file in parent. A sweep can remove the
// just-created, still empty directory between MkdirAll and here, so a missing
// directory is created once more before giving up.
func (s *dirStore) createTemp(parent string) (*os.File, error) {
	if s.beforeCreateTemp != nil {
		s.beforeCreateTemp(parent)
	}
	tmp, err := os.CreateTemp(parent, ".tmp-*")
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, err
		}
		return os.CreateTemp(parent, ".tmp-*")
	}
	return tmp, err
}

// writeAtomic writes sealed to a temporary file in parent and renames it to
// target. The temporary file is removed on any failure.
func (s *dirStore) writeAtomic(parent, target string, sealed []byte) error {
	tmp, err := s.createTemp(parent)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(sealed); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if s.beforeRename != nil {
		if err := s.beforeRename(tmpPath); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return err
	}
	renamed = true
	return nil
}

// Get reads, decrypts and checks one copy. Errors never carry file contents.
func (s *dirStore) Get(ctx context.Context, key, wantSHA256 string) ([]byte, error) {
	full, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("artifacts: read %s: %w", key, err)
	}
	if info.Size() > MaxObjectBytes+cipherOverhead {
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, key)
	}
	sealed, err := os.ReadFile(full) //nolint:gosec // full is confined to the store by resolve
	if err != nil {
		return nil, fmt.Errorf("artifacts: read %s: %w", key, err)
	}
	data, err := s.cipher.Decrypt(ctx, artifactPurpose, sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrCorrupt, key)
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), wantSHA256) {
		return nil, fmt.Errorf("%w: %s", ErrCorrupt, key)
	}
	return data, nil
}

// resolve maps a key to a path inside the store and refuses any key that
// could name a file outside it.
func (s *dirStore) resolve(key string) (string, error) {
	outside := fmt.Errorf("artifacts: key %q is outside %s", key, s.dir)
	if !strings.HasPrefix(key, keyPrefix) || path.Clean(key) != key || strings.Contains(key, "\\") || strings.Contains(key, "..") {
		return "", outside
	}
	full := filepath.Join(s.dir, filepath.FromSlash(key))
	rel, err := filepath.Rel(s.dir, full)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", outside
	}
	return full, nil
}
