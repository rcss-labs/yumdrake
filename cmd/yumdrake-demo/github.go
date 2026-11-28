package main

import (
	"fmt"

	"github.com/rcss-labs/yamdrake/test/e2e/fakegithub"
)

const (
	// demoGitHubToken is the bearer token of the fake GitHub. It is PUBLIC and opens nothing real.
	demoGitHubToken = "yumdrake-demo-fake-github-token" //nolint:gosec // token of the fake GitHub only
	demoOwner       = "demo-org"
	demoRepo        = demoOwner + "/platform"
	demoBranch      = "main"
	// demoMainCommit is the head of main; the inventory is active when it is the active commit.
	demoMainCommit = "1111111111111111111111111111111111111111"
)

// demoBranches are the branches of the fake repository. Every one holds a copy of the sample data
// under its own commit, so each can be told apart by its head.
var demoBranches = []struct{ name, commit string }{
	{demoBranch, demoMainCommit},
	{"dev", "2222222222222222222222222222222222222222"},
	{"v1.4.0", "3333333333333333333333333333333333333333"},
	{"v1.5.0", "4444444444444444444444444444444444444444"},
	{"v1.6.0", "5555555555555555555555555555555555555555"},
}

// newDemoGitHub builds the fake GitHub (not listening: serve its Handler) holding the sample data
// in dataDir on every branch of demoBranches.
func newDemoGitHub(dataDir string) (*fakegithub.Server, error) {
	gh := fakegithub.NewUnstarted(demoGitHubToken)
	for _, branch := range demoBranches {
		if err := gh.LoadDir(demoRepo, branch.commit, dataDir); err != nil {
			return nil, fmt.Errorf("load the sample data for %s: %w", branch.name, err)
		}
		gh.SetHead(demoRepo, branch.name, branch.commit)
	}
	return gh, nil
}
