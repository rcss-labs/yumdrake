package main

import (
	"path/filepath"
	"testing"

	"github.com/rcss-labs/yamdrake/internal/spec"
)

// demoDir is the sample data the demo serves; the image copies it to /demo.
const demoDir = "../../deploy/compose/demo"

// TestDemoFixtureIsValid runs the check `yumdrake validate` runs, so a sample
// that the product would reject cannot be committed.
func TestDemoFixtureIsValid(t *testing.T) {
	jobs, err := filepath.Glob(filepath.Join(demoDir, "jobs", "*.yaml"))
	if err != nil || len(jobs) == 0 {
		t.Fatalf("job files = %v, %v; want at least one", jobs, err)
	}
	inventory, loaded, problems := spec.LoadDir(demoDir, jobs)
	for _, problem := range problems {
		t.Errorf("problem: %v", spec.Problems{problem}.Error())
	}
	if len(problems) > 0 {
		return
	}
	if got := len(inventory.Environments); got != 5 {
		t.Errorf("environments = %d, want 5", got)
	}
	if got := len(inventory.Services); got < 4 || got > 6 {
		t.Errorf("services = %d, want 4 to 6", got)
	}
	if len(loaded) != len(jobs) {
		t.Errorf("loaded jobs = %d, want %d", len(loaded), len(jobs))
	}
}
