//go:build integration && floci

package copyconfig_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner/runnertest"
	"github.com/rcss-labs/yamdrake/internal/steps/copyconfig"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/test/awstest"
)

// TestCopyConfigAgainstFloci writes an env file and a TOML to destinations in
// a random account: the real artifact store holds the stored copies, and the
// bytes at the destinations must equal them.
func TestCopyConfigAgainstFloci(t *testing.T) {
	const region = "us-east-1"
	ctx := context.Background()
	w := awstest.New(t)
	acct := w.Account(awstest.RandomAccountID())
	sfx := awstest.RandomSuffix()

	artifactBucket, destBucket := "yd-art-"+sfx, "yd-dst-"+sfx
	acct.Bucket(artifactBucket, false)
	acct.Bucket(destBucket, false)
	acct.PutObject(destBucket, "web.env", []byte("KEY_A=old\n")) // replaced

	files := []struct {
		kind, name, key string
		content         []byte
	}{
		{"env", "web.env", "web.env", []byte("KEY_A=floci-new\nKEY_B=1\n")},
		{"toml", "web.toml", "conf/web.toml", []byte("token = \"floci-toml\"\n")},
	}
	stored := artifacts.New(s3.NewFromConfig(acct.AWSConfig(region), func(o *s3.Options) { o.UsePathStyle = true }), artifactBucket, "")
	var items []manifest.ConfigItem
	for _, f := range files {
		key, hash, err := stored.Put(ctx, 9002, "web", f.kind, f.name, false, f.content)
		if err != nil {
			t.Fatalf("store copy of %s: %v", f.name, err)
		}
		items = append(items, manifest.ConfigItem{
			Kind: f.kind, Name: f.name, Destination: "s3://" + destBucket + "/" + f.key,
			SHA256: hash, ArtifactKey: key, Size: int64(len(f.content)),
		})
	}

	raw, err := json.Marshal(manifest.Manifest{Services: []manifest.Service{{
		Name: "web", ECSService: fmt.Sprintf("arn:aws:ecs:%s:%s:service/c/web", region, acct.ID), Config: items,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	task := queue.Claimed{
		Leg: storeLeg(9002),
		Run: runRow(9002, acct.ID, raw),
	}
	env := &runnertest.Env{Attempt: 1}
	var sessions []string
	out := copyconfig.New(copyconfig.Deps{
		AWS: func(_ context.Context, account, r, session string) (aws.Config, error) {
			if account != acct.ID {
				return aws.Config{}, fmt.Errorf("unexpected account %s", account)
			}
			sessions = append(sessions, session)
			return acct.AWSConfig(r), nil
		},
		Artifacts: stored,
		Options:   awsx.Options{EndpointURL: w.Endpoint},
	}).Execute(ctx, env, task)

	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q: %s\n%s", out.State, out.FailureShort, out.FailureDetail, logText(env))
	}
	res := result(t, out)
	if len(res.Written) != 2 {
		t.Errorf("result = %+v", res)
	}
	for _, f := range files {
		got, ok := acct.Object(destBucket, f.key)
		if !ok || !bytes.Equal(got, f.content) {
			t.Errorf("%s at the destination = %q, %v; want %q", f.name, got, ok, f.content)
		}
	}
	for _, s := range sessions {
		if s != "yumdrake-runner-run-9002" {
			t.Errorf("session = %q", s)
		}
	}

	// A second run finds both in place and writes nothing.
	again := &runnertest.Env{Attempt: 2}
	out = copyconfig.New(copyconfig.Deps{
		AWS:       func(_ context.Context, _, r, _ string) (aws.Config, error) { return acct.AWSConfig(r), nil },
		Artifacts: stored,
		Options:   awsx.Options{EndpointURL: w.Endpoint},
	}).Execute(ctx, again, task)
	if res := result(t, out); out.State != "succeeded" || len(res.AlreadyInPlace) != 2 || len(again.OpsSnapshot()) != 0 {
		t.Errorf("second run = %q %+v, ops %d", out.State, res, len(again.OpsSnapshot()))
	}
}

func storeLeg(runID int64) store.Leg {
	return store.Leg{ID: 1, RunID: runID, Kind: "step", Service: "web", Attempt: 1}
}

func runRow(id int64, account string, manifestJSON []byte) store.Run {
	return store.Run{ID: id, AWSAccount: account, Manifest: manifestJSON}
}
