//go:build integration

package versions_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/cache"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/versions"
)

func openTestValkey(t *testing.T) valkey.Client {
	t.Helper()
	addr := os.Getenv("YUMDRAKE_TEST_VALKEY_ADDR")
	if addr == "" {
		t.Fatal("YUMDRAKE_TEST_VALKEY_ADDR must be set when running integration tests")
	}
	client, err := cache.Open(cache.Options{Addr: addr})
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestForJobCachesForSixtySecondsAndWorksWithoutValkey(t *testing.T) {
	ctx := context.Background()
	client := openTestValkey(t)
	prefix := fmt.Sprintf("versions-test-%d:", time.Now().UnixNano())
	const env = "prod-01"
	build := func(rule spec.VersionRule) (spec.Job, spec.Inventory) {
		return spec.Job{ID: "j", Groups: []spec.Group{{ID: "g", Services: []string{"alpha"}}}},
			spec.Inventory{Environments: []spec.Environment{{Name: env}},
				Services: map[string]spec.Service{"alpha": serviceIn("alpha", env, "111111111111", rule)}}
	}
	fleet := &fleetECR{images: map[string][]ecrtypes.ImageDetail{"alpha": fleetImages("1.0.0", "1.1.0")}}
	service := versions.New(versions.Config{
		Discovery: &fleetDiscovery{},
		NewClient: func(aws.Config) versions.ECR { return fleet },
		Valkey:    client, KeyPrefix: prefix,
	})
	keys := func() []string {
		t.Helper()
		out, err := client.Do(ctx, client.B().Keys().Pattern(prefix+"*").Build()).AsStrSlice()
		if err != nil {
			t.Fatalf("KEYS: %v", err)
		}
		return out
	}
	t.Cleanup(func() {
		for _, key := range keys() {
			client.Do(ctx, client.B().Del().Key(key).Build())
		}
	})

	job, inv := build(semverRule)
	first, err := service.ForJob(ctx, job, inv, env)
	if err != nil {
		t.Fatalf("ForJob: %v", err)
	}
	second, err := service.ForJob(ctx, job, inv, env)
	if err != nil {
		t.Fatalf("second ForJob: %v", err)
	}
	if calls := fleet.calls.Load(); calls != 1 {
		t.Errorf("ECR calls after two ForJob = %d, want 1 (second served from Valkey)", calls)
	}
	if got, want := tagsOf(second.Groups["g"].Services["alpha"]), tagsOf(first.Groups["g"].Services["alpha"]); fmt.Sprint(got) != fmt.Sprint(want) || len(got) != 2 {
		t.Errorf("cached list = %v, want %v", got, want)
	}
	if got := second.Groups["g"].Services["alpha"][0].PushedAt; !got.Equal(first.Groups["g"].Services["alpha"][0].PushedAt) {
		t.Errorf("cached push time = %v, want it to survive the cache", got)
	}

	// The key carries the prefix, account, region, repository and a hash of the rule.
	stored := keys()
	if len(stored) != 1 {
		t.Fatalf("keys = %v, want exactly one", stored)
	}
	wantStart := prefix + "versions:111111111111:eu-west-1:alpha:"
	if len(stored[0]) <= len(wantStart) || stored[0][:len(wantStart)] != wantStart {
		t.Errorf("key = %q, want it to start with %q", stored[0], wantStart)
	}
	ttl, err := client.Do(ctx, client.B().Pttl().Key(stored[0]).Build()).AsInt64()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl <= 50_000 || ttl > 60_000 {
		t.Errorf("TTL = %d ms, want just under 60000", ttl)
	}

	// A different rule must not be served from the old entry.
	jobLimited, invLimited := build(spec.VersionRule{Pattern: semverRule.Pattern, Sort: "semver", Limit: 1})
	limited, err := service.ForJob(ctx, jobLimited, invLimited, env)
	if err != nil {
		t.Fatalf("ForJob with a new rule: %v", err)
	}
	if fleet.calls.Load() != 2 || len(limited.Groups["g"].Services["alpha"]) != 1 || len(keys()) != 2 {
		t.Errorf("a changed rule: calls %d, versions %v, keys %d; want a fresh listing under a second key",
			fleet.calls.Load(), tagsOf(limited.Groups["g"].Services["alpha"]), len(keys()))
	}

	// After the entry expires (simulated by deleting it) ECR is asked again.
	client.Do(ctx, client.B().Del().Key(stored[0]).Build())
	if _, err := service.ForJob(ctx, job, inv, env); err != nil {
		t.Fatalf("ForJob after expiry: %v", err)
	}
	if calls := fleet.calls.Load(); calls != 3 {
		t.Errorf("ECR calls after expiry = %d, want 3", calls)
	}

	// With Valkey gone the list still works, uncached.
	dead, err := cache.Open(cache.Options{Addr: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	dead.Close()
	downFleet := &fleetECR{images: fleet.images}
	down := versions.New(versions.Config{
		Discovery: &fleetDiscovery{},
		NewClient: func(aws.Config) versions.ECR { return downFleet },
		Valkey:    dead, KeyPrefix: prefix,
	})
	for range 2 {
		got, err := down.ForJob(ctx, job, inv, env)
		if err != nil || len(got.Groups["g"].Services["alpha"]) != 2 || len(got.Groups["g"].Unavailable) != 0 {
			t.Fatalf("ForJob without Valkey = %+v, %v; want the list", got, err)
		}
	}
	if calls := downFleet.calls.Load(); calls != 2 {
		t.Errorf("ECR calls without Valkey = %d, want 2 (nothing cached)", calls)
	}
}
