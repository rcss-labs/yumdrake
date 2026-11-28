// Package artifacts keeps copies of the configuration files a run touched, and
// verifies their SHA-256 on every read.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrCorrupt means a stored object cannot be decrypted or does not match the
// checksum it was recorded with.
var ErrCorrupt = errors.New("artifacts: stored object does not match its checksum")

// ErrTooLarge means an object is over MaxObjectBytes.
var ErrTooLarge = errors.New("artifacts: object exceeds the size limit")

// MaxObjectBytes is the largest configuration copy that is stored.
const MaxObjectBytes = 16 << 20

const keyPrefix = "manifests/"

const previousSegment = "previous"

// Store holds configuration copies. Put returns the key and the SHA-256 (hex,
// lowercase) of the plaintext; Get refuses data whose checksum differs from
// wantSHA256.
type Store interface {
	// Put stores data for one file of one run. previous marks the copy of the
	// file as it was before the run changed it.
	Put(ctx context.Context, runID int64, service, kind, name string, previous bool, data []byte) (key, sha256hex string, err error)
	// Get returns the plaintext stored under key after checking it against wantSHA256.
	Get(ctx context.Context, key, wantSHA256 string) ([]byte, error)
}

// objectKey builds the key of one copy and rejects components that could
// escape the store or make two copies collide.
func objectKey(runID int64, service, kind, name string, previous bool) (string, error) {
	fields := []struct{ name, value string }{{"service", service}, {"kind", kind}, {"name", name}}
	for _, f := range fields {
		if err := validComponent(f.value); err != nil {
			return "", fmt.Errorf("artifacts: invalid %s: %w", f.name, err)
		}
	}
	// Previous copies live in a "previous" directory placed before the kind, so
	// no name can collide with it; the kind "previous" is reserved for that.
	if kind == previousSegment {
		return "", fmt.Errorf("artifacts: invalid kind: %q is reserved", previousSegment)
	}
	if previous {
		return fmt.Sprintf("%s%d/%s/%s/%s/%s", keyPrefix, runID, service, previousSegment, kind, name), nil
	}
	return fmt.Sprintf("%s%d/%s/%s/%s", keyPrefix, runID, service, kind, name), nil
}

// validComponent refuses a key component that is empty, could traverse
// directories, or holds control characters.
func validComponent(v string) error {
	switch {
	case v == "":
		return errors.New("must not be empty")
	case strings.Contains(v, "/"):
		return errors.New("must not contain '/'")
	case strings.Contains(v, "\\"):
		return errors.New("must not contain '\\'")
	case v == "." || strings.Contains(v, ".."):
		return errors.New("must not be '.' or contain '..'")
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}
