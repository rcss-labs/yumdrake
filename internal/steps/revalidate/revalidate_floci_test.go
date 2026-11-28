//go:build integration && floci

package revalidate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/github"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/steps/resolve"
	"github.com/rcss-labs/yamdrake/internal/steps/revalidate"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/test/awstest"
)

// TestRevalidateDetectsHandDeploy resolves a service against the emulator,
// shows that revalidating straight away passes, then deploys the service by
// hand (a new task definition revision and a service update) and changes the
// config destination, and shows that revalidating now fails as "changed since
// planning", naming the service and both revisions.
func TestRevalidateDetectsHandDeploy(t *testing.T) {
	const flociRegion = "us-east-1"
	ctx := context.Background()
	w := awstest.New(t)
	acct := w.Account(awstest.RandomAccountID())
	sfx := awstest.RandomSuffix()

	repo := fmt.Sprintf("yd-web-%s", sfx)
	repoURI, _ := acct.Repo(flociRegion, repo, "1.3.0", "1.4.2")
	cluster := fmt.Sprintf("yd-c-web-%s", sfx)
	serviceARN, taskDefARN := acct.Service(flociRegion, cluster, "web", repoURI+":1.3.0")

	sourceBucket, destBucket, artifactBucket := "yd-src-"+sfx, "yd-dst-"+sfx, "yd-art-"+sfx
	acct.Bucket(sourceBucket, false)
	acct.Bucket(destBucket, false)
	acct.Bucket(artifactBucket, false)
	toml, prevToml := []byte("token = \"revalidate-new\"\n"), []byte("token = \"revalidate-old\"\n")
	acct.PutObject(sourceBucket, "web.toml", toml)
	acct.PutObject(destBucket, "web.toml", prevToml)

	inv := spec.Inventory{
		Environments: []spec.Environment{{Name: "dev-1", Label: "dev", AWSAccount: acct.ID}},
		Services: map[string]spec.Service{"web": {Name: "web", Environments: map[string]spec.ServiceEnv{"dev-1": {
			ECSService: serviceARN, ECRRepository: repoURI,
			Tomls: []spec.Toml{{Name: "web.toml", Source: "s3://" + sourceBucket + "/web.toml", Destination: "s3://" + destBucket + "/web.toml"}},
		}}}},
	}
	artifactClient := s3.NewFromConfig(acct.AWSConfig(flociRegion), func(o *s3.Options) { o.UsePathStyle = true })
	deps := resolve.Deps{
		AWS: func(_ context.Context, accountID, region, _ string) (aws.Config, error) {
			if accountID != acct.ID {
				return aws.Config{}, fmt.Errorf("unexpected account %s", accountID)
			}
			return acct.AWSConfig(region), nil
		},
		Options: awsx.Options{EndpointURL: w.Endpoint},
		GitHub: func(context.Context, string) (resolve.Source, error) {
			return nil, github.ErrNotFound // no env files in this test
		},
		Artifacts: artifacts.NewS3(artifactClient, artifactBucket, ""),
		Inventory: func(context.Context, int64) (spec.Inventory, error) { return inv, nil },
	}

	selection, err := json.Marshal(engine.Selection{"g": {Version: "1.4.2", Services: map[string]string{"web": ""}}})
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{
		ID: 9002, Environment: "dev-1", Label: "dev", AWSAccount: acct.ID, InventorySnapshotID: 1,
		InventoryCommit: "inv", JobCommit: "job", Selection: selection,
	}
	resolved := resolve.New(deps).Execute(ctx, &testEnv{}, queue.Claimed{Leg: store.Leg{ID: 1, RunID: 9002, Kind: "resolve"}, Run: run})
	if resolved.State != "succeeded" {
		t.Fatalf("resolve = %q %q: %s", resolved.State, resolved.FailureShort, resolved.FailureDetail)
	}
	run.Manifest = resolved.Result
	task := queue.Claimed{Leg: store.Leg{ID: 2, RunID: 9002, Kind: "revalidate"}, Run: run}

	env := &testEnv{}
	if out := revalidate.New(deps).Execute(ctx, env, task); out.State != "succeeded" {
		t.Fatalf("revalidate before any change = %q %q: %s\nlog:\n%s", out.State, out.FailureShort, out.FailureDetail, env.text())
	}

	// Someone deploys by hand: a new revision of the task definition, and the
	// service moved onto it.
	client := ecs.NewFromConfig(acct.AWSConfig(flociRegion))
	td, err := client.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("web"), NetworkMode: ecstypes.NetworkModeAwsvpc,
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		Cpu:                     aws.String("256"), Memory: aws.String("512"),
		ContainerDefinitions: []ecstypes.ContainerDefinition{{Name: aws.String("app"), Image: aws.String(repoURI + ":1.4.2"), Essential: aws.Bool(true)}},
	})
	if err != nil || td.TaskDefinition == nil {
		t.Fatalf("RegisterTaskDefinition: %v", err)
	}
	if _, err := client.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster: aws.String(cluster), Service: aws.String("web"), TaskDefinition: td.TaskDefinition.TaskDefinitionArn,
	}); err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	handDeployed := aws.ToString(td.TaskDefinition.TaskDefinitionArn)

	env = &testEnv{}
	out := revalidate.New(deps).Execute(ctx, env, task)
	if out.State != "failed" || out.FailureShort != "changed since planning" {
		t.Fatalf("revalidate after hand deploy = %q %q: %s\nlog:\n%s", out.State, out.FailureShort, out.FailureDetail, env.text())
	}
	for _, want := range []string{"web", taskDefARN, handDeployed} {
		if !strings.Contains(out.FailureDetail, want) {
			t.Errorf("detail %q does not name %q", out.FailureDetail, want)
		}
	}
	if strings.Contains(out.FailureDetail, "image sha256:") { // the image itself is untouched
		t.Errorf("detail blames the image: %s", out.FailureDetail)
	}

	// A config destination changed by another tool is caught too.
	acct.PutObject(destBucket, "web.toml", []byte("token = \"revalidate-hand-edit\"\n"))
	env = &testEnv{}
	out = revalidate.New(deps).Execute(ctx, env, task)
	if !strings.Contains(out.FailureDetail, "web.toml") {
		t.Errorf("detail %q does not name the changed destination", out.FailureDetail)
	}
	for _, secret := range []string{"revalidate-new", "revalidate-old", "revalidate-hand-edit"} {
		if strings.Contains(env.text(), secret) || strings.Contains(out.FailureDetail, secret) {
			t.Errorf("file content %q leaked", secret)
		}
	}
}
