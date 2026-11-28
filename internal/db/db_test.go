package db

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// Migration numbers may have gaps (another branch owns 00006), so this checks
// that the latest version is at least the last one known to exist and that
// every embedded file is at or below it, not an exact number.
func TestLatestVersionMatchesEmbeddedFiles(t *testing.T) {
	const lastKnown = 12 // 00012_visibility.sql
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var highest int64
	for _, e := range entries {
		prefix, _, _ := strings.Cut(e.Name(), "_")
		n, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("migration %q does not start with a number", e.Name())
		}
		highest = max(highest, n)
	}
	if got := LatestVersion(); got != highest {
		t.Fatalf("LatestVersion() = %d, want %d (the highest numbered file in migrations/)", got, highest)
	}
	if highest < lastKnown {
		t.Fatalf("highest migration is %d, want at least %d", highest, lastKnown)
	}
}
