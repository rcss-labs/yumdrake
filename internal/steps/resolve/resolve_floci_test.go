//go:build integration && floci

package resolve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/github"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/steps/resolve"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/test/awstest"
	"github.com/rcss-labs/yamdrake/test/e2e/fakegithub"
)

// TestResolveAgainstFloci resolves two services against the emulator: real
// ECS services and ECR images, an env file from the fake GitHub, a TOML in a
// versioned bucket and previous destination content. It checks the manifest
// against what the emulator holds, that the copies in the artifact bucket hash
// correctly, and that no destination was touched.
func TestResolveAgainstFloci(t *testing.T) {
	const flociRegion = "us-east-1"
	ctx := context.Background()
	w := awstest.New(t)
	acct := w.Account(awstest.RandomAccountID())
	sfx := awstest.RandomSuffix()

	// Two repositories and running services. The services run 1.3.0 and the
	// run asks for 1.4.2; both tags point at one image, so a digest reported
	// per tag has to be grouped by digest in our code.
	var (
		repoURI   = map[string]string{}
		digest    = map[string]string{}
		serviceID = map[string]string{}
		taskDef   = map[string]string{}
	)
	for _, name := range []string{"web", "api"} {
		repo := fmt.Sprintf("yd-%s-%s", name, sfx)
		repoURI[name], digest[name] = acct.Repo(flociRegion, repo, "1.3.0", "1.4.2")
		cluster := fmt.Sprintf("yd-c-%s-%s", name, sfx)
		serviceID[name], taskDef[name] = acct.Service(flociRegion, cluster, name, repoURI[name]+":1.3.0")
	}

	// Config: a versioned source bucket for the TOML, a destination bucket
	// that already holds previous content, and the artifact bucket.
	sourceBucket, destBucket, artifactBucket := "yd-src-"+sfx, "yd-dst-"+sfx, "yd-art-"+sfx
	acct.Bucket(sourceBucket, true)
	acct.Bucket(destBucket, false)
	acct.Bucket(artifactBucket, false)
	toml, prevToml := []byte("token = \"flocitoml-new\"\n"), []byte("token = \"flocitoml-old\"\n")
	env, prevEnv := []byte("KEY_A=flocienv-new\nKEY_B=1\n"), []byte("KEY_A=flocienv-old\nKEY_C=1\n")
	acct.PutObject(sourceBucket, "web.toml", toml)
	acct.PutObject(destBucket, "web.toml", prevToml)
	acct.PutObject(destBucket, "web.env", prevEnv)

	gh := fakegithub.New("floci-test-token")
	t.Cleanup(gh.Close)
	gh.AddCommit("acme/config", "commit-1", map[string][]byte{"envs/web.env": env})
	gh.SetHead("acme/config", "v1.4.2", "commit-1")
	ghClient, err := github.New(gh.URL(), nil, github.Credentials{Method: "token", Token: "floci-test-token"})
	if err != nil {
		t.Fatalf("github client: %v", err)
	}

	inv := spec.Inventory{
		Environments: []spec.Environment{{Name: "dev-1", Label: "dev", AWSAccount: acct.ID}},
		Services:     map[string]spec.Service{},
	}
	for _, name := range []string{"web", "api"} {
		se := spec.ServiceEnv{ECSService: serviceID[name], ECRRepository: repoURI[name]}
		if name == "web" {
			se.EnvFiles = []spec.EnvFile{{Name: "web.env", Source: "acme/config/envs/web.env", Destination: "s3://" + destBucket + "/web.env"}}
			se.Tomls = []spec.Toml{{Name: "web.toml", Source: "s3://" + sourceBucket + "/web.toml", Destination: "s3://" + destBucket + "/web.toml"}}
		}
		inv.Services[name] = spec.Service{Name: name, Environments: map[string]spec.ServiceEnv{"dev-1": se}}
	}

	artifactClient := s3.NewFromConfig(acct.AWSConfig(flociRegion), func(o *s3.Options) { o.UsePathStyle = true })
	var sessions []string
	deps := resolve.Deps{
		AWS: func(_ context.Context, accountID, region, session string) (aws.Config, error) {
			if accountID != acct.ID {
				return aws.Config{}, fmt.Errorf("unexpected account %s", accountID)
			}
			sessions = append(sessions, session)
			return acct.AWSConfig(region), nil
		},
		Options:   awsx.Options{EndpointURL: w.Endpoint},
		GitHub:    func(context.Context, string) (resolve.Source, error) { return ghClient, nil },
		Artifacts: artifacts.NewS3(artifactClient, artifactBucket, ""),
		Inventory: func(context.Context, int64) (spec.Inventory, error) { return inv, nil },
	}

	selection, err := json.Marshal(engine.Selection{"g": {Version: "1.4.2", Services: map[string]string{"web": "", "api": ""}}})
	if err != nil {
		t.Fatal(err)
	}
	task := queue.Claimed{
		Leg: store.Leg{ID: 1, RunID: 9001, Kind: "resolve"},
		Run: store.Run{
			ID: 9001, Environment: "dev-1", Label: "dev", AWSAccount: acct.ID, InventorySnapshotID: 1,
			InventoryCommit: "inv", JobCommit: "job", Selection: selection,
		},
	}
	tenv := &testEnv{}
	out := resolve.New(deps).Execute(ctx, tenv, task)
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q: %s\nlog:\n%s", out.State, out.FailureShort, out.FailureDetail, tenv.text())
	}
	var m manifest.Manifest
	if err := json.Unmarshal(out.Result, &m); err != nil {
		t.Fatalf("result: %v", err)
	}

	for _, s := range m.Services {
		if s.ImageDigest != digest[s.Name] || !strings.HasPrefix(s.ImageDigest, "sha256:") {
			t.Errorf("%s digest = %q, want %q", s.Name, s.ImageDigest, digest[s.Name])
		}
		if s.PreviousTaskDefinition != taskDef[s.Name] || s.ECSService != serviceID[s.Name] {
			t.Errorf("%s previous task definition = %q, want %q", s.Name, s.PreviousTaskDefinition, taskDef[s.Name])
		}
		if s.PreviousVersion != "1.3.0" {
			t.Errorf("%s previous version = %q, want 1.3.0", s.Name, s.PreviousVersion)
		}
	}
	for _, c := range m.Checks {
		want := "passed"
		if c.Name == "write permission" {
			want = "not_verified"
		}
		if c.Status != want {
			t.Errorf("check %s/%s = %s (%s), want %s", c.Service, c.Name, c.Status, c.Detail, want)
		}
	}

	var web manifest.Service
	for _, s := range m.Services {
		if s.Name == "web" {
			web = s
		}
	}
	if len(web.Config) != 2 {
		t.Fatalf("web config = %+v", web)
	}
	for i, item := range web.Config {
		wantNew, wantPrev := env, prevEnv
		if item.Kind == "toml" {
			wantNew, wantPrev = toml, prevToml
		}
		if item.SHA256 != sha(wantNew) || item.PreviousSHA256 != sha(wantPrev) || item.Size != int64(len(wantNew)) || item.Change != "changed" {
			t.Errorf("config[%d] = %+v", i, item)
		}
		if got, ok := acct.Object(artifactBucket, item.ArtifactKey); !ok || !bytes.Equal(got, wantNew) {
			t.Errorf("config[%d] artifact %q = %q, %v", i, item.ArtifactKey, got, ok)
		}
		if got, ok := acct.Object(artifactBucket, item.PreviousArtifactKey); !ok || !bytes.Equal(got, wantPrev) {
			t.Errorf("config[%d] previous artifact %q = %q, %v", i, item.PreviousArtifactKey, got, ok)
		}
		if item.Kind == "env" {
			if item.Branch != "v1.4.2" || item.Commit != "commit-1" || item.Keys == nil ||
				strings.Join(item.Keys.Added, ",") != "KEY_B" || strings.Join(item.Keys.Removed, ",") != "KEY_C" || strings.Join(item.Keys.Changed, ",") != "KEY_A" {
				t.Errorf("env item = %+v", item)
			}
		} else if item.SourceVersionID == "" {
			t.Errorf("the toml source bucket is versioned, but no version id was recorded: %+v", item)
		}
	}

	// A read-only task: the destinations hold what they held before.
	if got, _ := acct.Object(destBucket, "web.env"); !bytes.Equal(got, prevEnv) {
		t.Errorf("destination env changed to %q", got)
	}
	if got, _ := acct.Object(destBucket, "web.toml"); !bytes.Equal(got, prevToml) {
		t.Errorf("destination toml changed to %q", got)
	}
	for _, s := range sessions {
		if s != "yumdrake-runner-run-9001" {
			t.Errorf("session name = %q", s)
		}
	}
	for _, secret := range []string{"flocienv-new", "flocienv-old", "flocitoml-new", "flocitoml-old"} {
		if strings.Contains(tenv.text(), secret) || strings.Contains(string(out.Result), secret) {
			t.Errorf("file content %q leaked into the log or the result", secret)
		}
	}
}
