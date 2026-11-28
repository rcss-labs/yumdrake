package resolve_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/github"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/steps/resolve"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const (
	account     = "111122223333"
	region      = "eu-west-1"
	runID       = 42
	snapshotID  = 7
	webARN      = "arn:aws:ecs:eu-west-1:111122223333:service/main/web"
	apiARN      = "arn:aws:ecs:eu-west-1:111122223333:service/main/api"
	webRepoURI  = "111122223333.dkr.ecr.eu-west-1.amazonaws.com/web"
	apiRepoURI  = "111122223333.dkr.ecr.eu-west-1.amazonaws.com/api"
	webTaskDef  = "arn:aws:ecs:eu-west-1:111122223333:task-definition/web:5"
	apiTaskDef  = "arn:aws:ecs:eu-west-1:111122223333:task-definition/api:9"
	webDigest   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	apiDigest   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	envSource   = "acme/config/prod/web/web.env"
	configRepo  = "acme/config"
	envPath     = "prod/web/web.env"
	branchHead  = "commit-v1-4-2"
	devHead     = "commit-dev"
	tomlBucket  = "config-source"
	tomlKey     = "prod/web.toml"
	destEnvURI  = "s3://config-dest/env/web.env"
	destTomlURI = "s3://config-dest/toml/web.toml"
)

// Values that must never reach a log line or the result.
const (
	secretEnvValue     = "hunter2-new-env-value" //nolint:gosec // fixture value, not a credential
	secretTomlValue    = "toml-secret-new-value"
	secretPrevEnvValue = "old-env-secret-111" //nolint:gosec // fixture value, not a credential
	secretPrevToml     = "old-toml-secret-222"
)

var (
	envContent      = []byte("API_KEY=" + secretEnvValue + "\nSHARED=same\nONLY_NEW=1\n")
	prevEnvContent  = []byte("API_KEY=" + secretPrevEnvValue + "\nSHARED=same\nONLY_OLD=1\n")
	tomlContent     = []byte("password = \"" + secretTomlValue + "\"\n")
	prevTomlContent = []byte("password = \"" + secretPrevToml + "\"\n")
)

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- stub AWS: ECS, ECR and S3 behind the package's small interfaces -------

type stubAWS struct {
	mu       sync.Mutex
	services map[string]ecstypes.Service // by service name
	taskDefs map[string]ecs.DescribeTaskDefinitionOutput
	images   map[string][]ecrtypes.ImageDetail // by "repo:tag"
	objects  map[string]stubObject             // by "bucket/key"
	calls    []string
	// callerAccount is what GetCallerIdentity answers; "" means the run's account.
	callerAccount string
	// hangECS makes DescribeServices wait for its context, like a stalled connection.
	hangECS bool
}

type stubObject struct {
	data      []byte
	versionID string
}

func (s *stubAWS) record(call string) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
}

func (s *stubAWS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	s.record("sts:GetCallerIdentity")
	if s.callerAccount != "" {
		return &sts.GetCallerIdentityOutput{Account: aws.String(s.callerAccount)}, nil
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String(account)}, nil
}

func (s *stubAWS) DescribeServices(ctx context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	s.record("ecs:DescribeServices")
	if s.hangECS {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	out := &ecs.DescribeServicesOutput{}
	for _, name := range in.Services {
		if svc, ok := s.services[name]; ok {
			out.Services = append(out.Services, svc)
		} else {
			out.Failures = append(out.Failures, ecstypes.Failure{Arn: aws.String(name), Reason: aws.String("MISSING")})
		}
	}
	return out, nil
}

func (s *stubAWS) DescribeTaskDefinition(_ context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	s.record("ecs:DescribeTaskDefinition")
	td, ok := s.taskDefs[aws.ToString(in.TaskDefinition)]
	if !ok {
		// What ECS answers for a task definition that does not exist.
		return nil, &smithy.GenericAPIError{Code: "ClientException", Message: "Unable to describe task definition."}
	}
	return &td, nil
}

func (s *stubAWS) DescribeImages(_ context.Context, in *ecr.DescribeImagesInput, _ ...func(*ecr.Options)) (*ecr.DescribeImagesOutput, error) {
	s.record("ecr:DescribeImages")
	var details []ecrtypes.ImageDetail
	for _, id := range in.ImageIds {
		got, ok := s.images[aws.ToString(in.RepositoryName)+":"+aws.ToString(id.ImageTag)]
		if !ok {
			return nil, &ecrtypes.ImageNotFoundException{Message: aws.String("not found")}
		}
		details = append(details, got...)
	}
	return &ecr.DescribeImagesOutput{ImageDetails: details}, nil
}

func (s *stubAWS) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	s.record("s3:GetObject")
	obj, ok := s.objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{Message: aws.String("no such key")}
	}
	out := &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(obj.data)), ContentLength: aws.Int64(int64(len(obj.data)))}
	if obj.versionID != "" {
		out.VersionId = aws.String(obj.versionID)
	}
	return out, nil
}

func (s *stubAWS) callCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == name {
			n++
		}
	}
	return n
}

func newStubAWS() *stubAWS {
	taskDef := func(arn, family, image string, tags map[string]string) ecs.DescribeTaskDefinitionOutput {
		out := ecs.DescribeTaskDefinitionOutput{TaskDefinition: &ecstypes.TaskDefinition{
			TaskDefinitionArn:    aws.String(arn),
			Family:               aws.String(family),
			ContainerDefinitions: []ecstypes.ContainerDefinition{{Name: aws.String("app"), Image: aws.String(image)}},
		}}
		for k, v := range tags {
			out.Tags = append(out.Tags, ecstypes.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		return out
	}
	active := func(arn, td string) ecstypes.Service {
		return ecstypes.Service{ServiceArn: aws.String(arn), Status: aws.String("ACTIVE"), TaskDefinition: aws.String(td)}
	}
	image := func(digest string, tags ...string) []ecrtypes.ImageDetail {
		return []ecrtypes.ImageDetail{{ImageDigest: aws.String(digest), ImageTags: tags}}
	}
	return &stubAWS{
		services: map[string]ecstypes.Service{"web": active(webARN, webTaskDef), "api": active(apiARN, apiTaskDef)},
		taskDefs: map[string]ecs.DescribeTaskDefinitionOutput{
			webTaskDef: taskDef(webTaskDef, "web", webRepoURI+":1.3.0", nil),
			apiTaskDef: taskDef(apiTaskDef, "api", apiRepoURI+":stale", map[string]string{"version": "1.3.9"}),
		},
		images: map[string][]ecrtypes.ImageDetail{
			"web:1.4.2": image(webDigest, "1.4.2"),
			"api:1.4.3": image(apiDigest, "1.4.3"),
		},
		objects: map[string]stubObject{
			tomlBucket + "/" + tomlKey:  {data: tomlContent, versionID: "v-123"},
			"config-dest/env/web.env":   {data: prevEnvContent},
			"config-dest/toml/web.toml": {data: prevTomlContent},
		},
	}
}

// --- stub GitHub, artifacts, env ------------------------------------------

type stubGitHub struct {
	mu       sync.Mutex
	branches map[string]string // branch -> head commit
	files    map[string][]byte // commit + ":" + path
	heads    []string          // branches HeadCommit was asked for
}

func newStubGitHub() *stubGitHub {
	return &stubGitHub{
		branches: map[string]string{"v1.4.2": branchHead, "dev": devHead},
		files:    map[string][]byte{branchHead + ":" + envPath: envContent, devHead + ":" + envPath: envContent},
	}
}

func (g *stubGitHub) BranchExists(_ context.Context, repo, branch string) (bool, error) {
	if repo != configRepo {
		return false, github.ErrNotFound
	}
	_, ok := g.branches[branch]
	return ok, nil
}

func (g *stubGitHub) HeadCommit(_ context.Context, _, branch string) (string, error) {
	g.mu.Lock()
	g.heads = append(g.heads, branch)
	g.mu.Unlock()
	commit, ok := g.branches[branch]
	if !ok {
		return "", github.ErrNotFound
	}
	return commit, nil
}

func (g *stubGitHub) ReadFile(_ context.Context, _, commit, path string) ([]byte, error) {
	data, ok := g.files[commit+":"+path]
	if !ok {
		return nil, fmt.Errorf("github: GET contents: %w", github.ErrNotFound)
	}
	return data, nil
}

type stubArtifacts struct {
	mu    sync.Mutex
	items map[string][]byte
	fail  error // returned by every Put when set
}

func newStubArtifacts() *stubArtifacts { return &stubArtifacts{items: map[string][]byte{}} }

func (a *stubArtifacts) Put(_ context.Context, run int64, service, kind, name string, previous bool, data []byte) (string, string, error) {
	if a.fail != nil {
		return "", "", a.fail
	}
	key := fmt.Sprintf("manifests/%d/%s/%s/", run, service, kind)
	if previous {
		key += "previous/"
	}
	key += name
	a.mu.Lock()
	a.items[key] = append([]byte(nil), data...)
	a.mu.Unlock()
	return key, sha(data), nil
}

// Get reads a copy back, as *artifacts.Store does: a key that is not there is
// the S3 "no such key" error, and bytes that do not hash to the checksum are
// artifacts.ErrCorrupt.
func (a *stubArtifacts) Get(_ context.Context, key, wantSHA256 string) ([]byte, error) {
	a.mu.Lock()
	data, ok := a.items[key]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("artifacts: get %s: %w", key, &s3types.NoSuchKey{Message: aws.String("no such key")})
	}
	if sha(data) != wantSHA256 {
		return nil, fmt.Errorf("%w: %s", artifacts.ErrCorrupt, key)
	}
	return append([]byte(nil), data...), nil
}

type testEnv struct {
	mu     sync.Mutex
	lines  []string
	cancel bool
}

func (e *testEnv) Log(level, text string) {
	e.mu.Lock()
	e.lines = append(e.lines, level+": "+text)
	e.mu.Unlock()
}
func (e *testEnv) Resume(any) (bool, error)                { return false, nil }
func (e *testEnv) Checkpoint(context.Context, any) error   { return nil }
func (e *testEnv) Ops(context.Context) ([]store.Op, error) { return nil, nil }
func (e *testEnv) CancelRequested(context.Context) bool    { return e.cancel }
func (e *testEnv) Do(context.Context, string, string, any, func() (any, error)) (store.Op, error) {
	panic("resolve must not use Env.Do")
}

func (e *testEnv) text() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.lines, "\n")
}

// --- fixture ----------------------------------------------------------------

func inventory() spec.Inventory {
	return spec.Inventory{
		Environments: []spec.Environment{{Name: "prod-1", Label: "prod", AWSAccount: account}},
		Services: map[string]spec.Service{
			"web": {Name: "web", Environments: map[string]spec.ServiceEnv{"prod-1": {
				ECSService: webARN, ECRRepository: webRepoURI,
				EnvFiles: []spec.EnvFile{{Name: "web.env", Source: envSource, Destination: destEnvURI}},
				Tomls:    []spec.Toml{{Name: "web.toml", Source: "s3://" + tomlBucket + "/" + tomlKey, Destination: destTomlURI}},
			}}},
			"api": {Name: "api", Environments: map[string]spec.ServiceEnv{"prod-1": {
				ECSService: apiARN, ECRRepository: apiRepoURI,
			}}},
		},
	}
}

func claimed(t *testing.T, label string, sel engine.Selection) queue.Claimed {
	t.Helper()
	selection, err := json.Marshal(sel)
	if err != nil {
		t.Fatal(err)
	}
	return queue.Claimed{
		Leg: store.Leg{ID: 1, RunID: runID, Kind: "resolve"},
		Run: store.Run{
			ID: runID, Environment: "prod-1", Label: label, AWSAccount: account,
			InventoryCommit: "inv-commit", JobCommit: "job-commit", InventorySnapshotID: snapshotID,
			Selection: selection,
		},
	}
}

func defaultSelection() engine.Selection {
	return engine.Selection{"all": {Version: "1.4.2", Services: map[string]string{"web": "", "api": "1.4.3"}}}
}

type harness struct {
	aws       *stubAWS
	gh        *stubGitHub
	artifacts *stubArtifacts
	env       *testEnv
	deps      resolve.Deps
	sessions  []string
	snapshots []int64
}

func newHarness() *harness {
	h := &harness{aws: newStubAWS(), gh: newStubGitHub(), artifacts: newStubArtifacts(), env: &testEnv{}}
	h.deps = resolve.Deps{
		AWS: func(_ context.Context, accountID, region, session string) (aws.Config, error) {
			if accountID != account {
				return aws.Config{}, fmt.Errorf("unexpected account %s", accountID)
			}
			h.sessions = append(h.sessions, session)
			return aws.Config{Region: region}, nil
		},
		Clients: func(aws.Config) resolve.Clients {
			return resolve.Clients{ECS: h.aws, ECR: h.aws, S3: h.aws, STS: h.aws}
		},
		GitHub:    func(context.Context, string) (resolve.Source, error) { return h.gh, nil },
		Artifacts: h.artifacts,
		Inventory: func(_ context.Context, id int64) (spec.Inventory, error) {
			h.snapshots = append(h.snapshots, id)
			return inventory(), nil
		},
	}
	return h
}

func (h *harness) run(t *testing.T, task queue.Claimed) (queue.Outcome, manifest.Manifest) {
	t.Helper()
	out := resolve.New(h.deps).Execute(context.Background(), h.env, task)
	var m manifest.Manifest
	if len(out.Result) > 0 {
		if err := json.Unmarshal(out.Result, &m); err != nil {
			t.Fatalf("result is not a manifest: %v", err)
		}
	}
	return out, m
}

func check(m manifest.Manifest, service, name string) (manifest.Check, bool) {
	for _, c := range m.Checks {
		if c.Service == service && c.Name == name {
			return c, true
		}
	}
	return manifest.Check{}, false
}

func serviceOf(t *testing.T, m manifest.Manifest, name string) manifest.Service {
	t.Helper()
	for _, s := range m.Services {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("manifest has no service %q", name)
	return manifest.Service{}
}

// --- branch -----------------------------------------------------------------

func TestBranchFor(t *testing.T) {
	for version, want := range map[string]string{"1.4.2": "v1.4.2", "v1.4.2": "v1.4.2", "2025-01": "v2025-01", "vnext": "vnext"} {
		if got := resolve.BranchFor(version); got != want {
			t.Errorf("BranchFor(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestChooseBranch(t *testing.T) {
	tests := []struct {
		name         string
		exists       bool
		label        string
		wantBranch   string
		wantFallback bool
		wantOK       bool
	}{
		{"exists in dev", true, "dev", "", false, true},
		{"exists in preprod", true, "preprod", "", false, true},
		{"exists in prod", true, "prod", "", false, true},
		{"missing in dev falls back", false, "dev", "dev", true, true},
		{"missing in preprod fails", false, "preprod", "", false, false},
		{"missing in prod fails", false, "prod", "", false, false},
		{"missing in an unknown label fails", false, "staging", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch, fallback, ok := resolve.ChooseBranch(tt.exists, tt.label)
			if branch != tt.wantBranch || fallback != tt.wantFallback || ok != tt.wantOK {
				t.Errorf("ChooseBranch(%v, %q) = (%q, %v, %v), want (%q, %v, %v)",
					tt.exists, tt.label, branch, fallback, ok, tt.wantBranch, tt.wantFallback, tt.wantOK)
			}
		})
	}
}

// --- resolve ----------------------------------------------------------------

func TestResolveHappyPathBuildsManifest(t *testing.T) {
	h := newHarness()
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))

	if out.State != "succeeded" {
		t.Fatalf("State = %q (%s: %s), want succeeded", out.State, out.FailureShort, out.FailureDetail)
	}
	if m.ID == "" || m.ID != manifest.Identity(m) {
		t.Errorf("manifest ID %q is not its identity %q", m.ID, manifest.Identity(m))
	}
	if m.RunID != runID || m.Environment != "prod-1" || m.Label != "prod" || m.AWSAccount != account ||
		m.InventoryCommit != "inv-commit" || m.JobCommit != "job-commit" {
		t.Errorf("manifest header = %+v", m)
	}
	if len(h.snapshots) != 1 || h.snapshots[0] != snapshotID {
		t.Errorf("inventory read for snapshots %v, want only %d", h.snapshots, snapshotID)
	}
	for _, s := range h.sessions {
		if s != fmt.Sprintf("yumdrake-runner-run-%d", runID) {
			t.Errorf("session name = %q", s)
		}
	}
	if len(m.Services) != 2 || m.Services[0].Name != "api" || m.Services[1].Name != "web" {
		t.Fatalf("services = %+v, want api then web", m.Services)
	}

	web := serviceOf(t, m, "web")
	if web.Version != "1.4.2" || web.ImageDigest != webDigest || web.ECSService != webARN || web.ECRRepository != webRepoURI ||
		web.PreviousTaskDefinition != webTaskDef || web.PreviousVersion != "1.3.0" || web.Group != "all" {
		t.Errorf("web = %+v", web)
	}
	api := serviceOf(t, m, "api")
	if api.Version != "1.4.3" || api.ImageDigest != apiDigest || api.PreviousTaskDefinition != apiTaskDef || api.PreviousVersion != "1.3.9" {
		t.Errorf("api = %+v (override version, digest, and the version tag on the task definition)", api)
	}
	if len(api.Config) != 0 {
		t.Errorf("api has config %+v", api.Config)
	}

	if len(web.Config) != 2 || web.Config[0].Kind != "env" || web.Config[1].Kind != "toml" {
		t.Fatalf("web config = %+v, want env then toml", web.Config)
	}
	env, toml := web.Config[0], web.Config[1]
	if env.Name != "web.env" || env.Source != envSource || env.Destination != destEnvURI ||
		env.Branch != "v1.4.2" || env.Commit != branchHead || env.Fallback ||
		env.SHA256 != sha(envContent) || env.Size != int64(len(envContent)) ||
		env.PreviousExists != true || env.PreviousSHA256 != sha(prevEnvContent) || env.Change != "changed" {
		t.Errorf("env item = %+v", env)
	}
	if env.Keys == nil || strings.Join(env.Keys.Added, ",") != "ONLY_NEW" ||
		strings.Join(env.Keys.Removed, ",") != "ONLY_OLD" || strings.Join(env.Keys.Changed, ",") != "API_KEY" {
		t.Errorf("env keys = %+v", env.Keys)
	}
	if toml.SourceVersionID != "v-123" || toml.SHA256 != sha(tomlContent) || toml.Change != "changed" ||
		toml.PreviousSHA256 != sha(prevTomlContent) || toml.Keys != nil || toml.Branch != "" {
		t.Errorf("toml item = %+v", toml)
	}
	// Copies sit under deterministic keys and hold exactly the bytes hashed.
	for key, want := range map[string][]byte{
		env.ArtifactKey: envContent, env.PreviousArtifactKey: prevEnvContent,
		toml.ArtifactKey: tomlContent, toml.PreviousArtifactKey: prevTomlContent,
	} {
		if key == "" || !bytes.Equal(h.artifacts.items[key], want) {
			t.Errorf("artifact %q holds %q, want %q", key, h.artifacts.items[key], want)
		}
	}

	for _, svc := range []string{"web", "api"} {
		for _, name := range []string{"service exists", "image exists"} {
			if c, ok := check(m, svc, name); !ok || c.Status != "passed" {
				t.Errorf("check %s/%s = %+v, %v", svc, name, c, ok)
			}
		}
		if c, ok := check(m, svc, "write permission"); !ok || c.Status != "not_verified" {
			t.Errorf("check %s/write permission = %+v, %v", svc, c, ok)
		}
	}
	for _, name := range []string{"config source web.env", "config source web.toml", "destination readable web.env", "destination readable web.toml"} {
		if c, ok := check(m, "web", name); !ok || c.Status != "passed" {
			t.Errorf("check web/%s = %+v, %v", name, c, ok)
		}
	}
	if len(m.Failed()) != 0 {
		t.Errorf("failed checks: %+v", m.Failed())
	}
}

func TestNewDestinationIsRecordedAsNew(t *testing.T) {
	h := newHarness()
	delete(h.aws.objects, "config-dest/env/web.env")
	_, m := h.run(t, claimed(t, "prod", defaultSelection()))
	env := serviceOf(t, m, "web").Config[0]
	if env.PreviousExists || env.Change != "new" || env.PreviousArtifactKey != "" || env.Keys != nil {
		t.Errorf("env item = %+v, want a new destination without previous copy or key diff", env)
	}
	if c, _ := check(m, "web", "destination readable web.env"); c.Status != "passed" {
		t.Errorf("an absent destination must pass the readable check: %+v", c)
	}
}

func TestMissingTagIsFailedCheckAndLegFails(t *testing.T) {
	h := newHarness()
	delete(h.aws.images, "web:1.4.2")
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))

	if out.State != "failed" || out.FailureShort != "preflight failed: 1 checks" {
		t.Fatalf("outcome = %q %q, want failed with %q", out.State, out.FailureShort, "preflight failed: 1 checks")
	}
	if len(out.Result) == 0 || m.ID == "" {
		t.Fatalf("a failed preflight still returns the manifest, got %q", out.Result)
	}
	c, ok := check(m, "web", "image exists")
	if !ok || c.Status != "failed" || !strings.Contains(c.Detail, "1.4.2") {
		t.Errorf("image check = %+v, %v", c, ok)
	}
}

func TestMissingBranchInProdFailsAndInDevFallsBack(t *testing.T) {
	sel := engine.Selection{"all": {Version: "1.4.2", Services: map[string]string{"web": ""}}}

	for _, label := range []string{"prod", "preprod"} {
		t.Run(label+" fails and never reads dev", func(t *testing.T) {
			h := newHarness()
			delete(h.gh.branches, "v1.4.2")
			out, m := h.run(t, claimed(t, label, sel))
			if out.State != "failed" || out.FailureShort != "preflight failed: 1 checks" {
				t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
			}
			c, _ := check(m, "web", "config source web.env")
			if c.Status != "failed" || !strings.Contains(c.Detail, "v1.4.2") {
				t.Errorf("check = %+v, want a failure naming the branch v1.4.2", c)
			}
			for _, branch := range h.gh.heads {
				if branch == "dev" {
					t.Error("prod must not read the dev branch")
				}
			}
			if env := serviceOf(t, m, "web").Config[0]; env.Fallback || env.Commit != "" {
				t.Errorf("env item = %+v, want no fallback and no commit", env)
			}
		})
	}

	t.Run("dev falls back and says so", func(t *testing.T) {
		h := newHarness()
		delete(h.gh.branches, "v1.4.2")
		out, m := h.run(t, claimed(t, "dev", sel))
		if out.State != "succeeded" {
			t.Fatalf("outcome = %q %q %q", out.State, out.FailureShort, out.FailureDetail)
		}
		env := serviceOf(t, m, "web").Config[0]
		if env.Branch != "dev" || !env.Fallback || env.Commit != devHead {
			t.Errorf("env item = %+v, want fallback to dev at %s", env, devHead)
		}
		if c, _ := check(m, "web", "config source web.env"); !strings.Contains(c.Detail, "fell back") {
			t.Errorf("detail %q should say the fallback happened", c.Detail)
		}
	})
}

func TestMissingFileInChosenBranchFails(t *testing.T) {
	h := newHarness()
	delete(h.gh.files, branchHead+":"+envPath)
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))
	if out.State != "failed" || out.FailureShort != "preflight failed: 1 checks" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	c, _ := check(m, "web", "config source web.env")
	if c.Status != "failed" || !strings.Contains(c.Detail, envPath) || !strings.Contains(c.Detail, "v1.4.2") {
		t.Errorf("check = %+v, want a failure naming the path and branch", c)
	}
	// What the new file would change is not known, and the plan says so.
	for _, item := range serviceOf(t, m, "web").Config {
		if item.Name == "web.env" && item.Change != "unknown" {
			t.Errorf("change of the item whose source failed = %q, want \"unknown\"", item.Change)
		}
	}
}

func TestMissingTomlSourceAndUnreadableServiceFail(t *testing.T) {
	h := newHarness()
	delete(h.aws.objects, tomlBucket+"/"+tomlKey)
	delete(h.aws.services, "api")
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))
	if out.State != "failed" || out.FailureShort != "preflight failed: 2 checks" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	if c, _ := check(m, "web", "config source web.toml"); c.Status != "failed" {
		t.Errorf("toml source check = %+v", c)
	}
	if c, _ := check(m, "api", "service exists"); c.Status != "failed" {
		t.Errorf("service check = %+v", c)
	}
}

func TestServiceThatIsNotActiveIsFailedCheck(t *testing.T) {
	h := newHarness()
	svc := h.aws.services["web"]
	svc.Status = aws.String("DRAINING")
	h.aws.services["web"] = svc
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))
	c, _ := check(m, "web", "service exists")
	if out.State != "failed" || c.Status != "failed" || !strings.Contains(c.Detail, "DRAINING") {
		t.Errorf("outcome %q, check %+v", out.State, c)
	}
}

func TestResolveRefusesAServiceInAnotherAccount(t *testing.T) {
	h := newHarness()
	svc := h.aws.services["web"]
	svc.ServiceArn = aws.String("arn:aws:ecs:eu-west-1:999999999999:service/main/web")
	h.aws.services["web"] = svc
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))
	c, _ := check(m, "web", "service exists")
	if out.State != "failed" || c.Status != "failed" || !strings.Contains(c.Detail, "999999999999") || !strings.Contains(c.Detail, account) {
		t.Errorf("outcome %q, check %+v; want a failed check naming both accounts", out.State, c)
	}
	if got := serviceOf(t, m, "web").PreviousTaskDefinition; got != "" {
		t.Errorf("previous task definition %q was pinned from another account", got)
	}
}

func TestResolveFailsWhenTheDeployCredentialSignsInToAnotherAccount(t *testing.T) {
	h := newHarness()
	h.aws.callerAccount = "999999999999"
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))
	c, _ := check(m, "web", "service exists")
	if out.State != "failed" || c.Status != "failed" || !strings.Contains(c.Detail, "999999999999") || !strings.Contains(c.Detail, account) {
		t.Errorf("outcome %q, check %+v; want a failed check naming both accounts", out.State, c)
	}
	if h.aws.callCount("ecs:DescribeServices") != 0 {
		t.Errorf("a service was read with a credential of another account")
	}
}

func TestRepositoryInAnotherAccountIsAllowed(t *testing.T) {
	h := newHarness()
	inv := inventory()
	svc := inv.Services["web"]
	env := svc.Environments["prod-1"]
	env.ECRRepository = "999999999999.dkr.ecr.eu-west-1.amazonaws.com/web"
	svc.Environments["prod-1"] = env
	inv.Services["web"] = svc
	h.deps.Inventory = func(context.Context, int64) (spec.Inventory, error) { return inv, nil }
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))
	if out.State != "succeeded" {
		t.Fatalf("State = %q (%s: %s), want succeeded", out.State, out.FailureShort, out.FailureDetail)
	}
	if c, _ := check(m, "web", "image exists"); c.Status != "passed" {
		t.Errorf("image check = %+v, want passed", c)
	}
}

func TestAWSCallsStopAtTheirDeadline(t *testing.T) {
	old := resolve.CallTimeout
	resolve.CallTimeout = 50 * time.Millisecond
	defer func() { resolve.CallTimeout = old }()
	h := newHarness()
	h.aws.hangECS = true
	done := make(chan struct{})
	var out queue.Outcome
	var m manifest.Manifest
	go func() { out, m = h.run(t, claimed(t, "prod", defaultSelection())); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resolve is still waiting on a stalled AWS call")
	}
	if c, _ := check(m, "web", "service exists"); out.State != "failed" || c.Status != "failed" || !strings.Contains(c.Detail, "Timeout") {
		t.Errorf("outcome %q, check %+v; want a failed check with the code Timeout", out.State, c)
	}
}

func TestOneDigestUnderSeveralEntriesIsOneImage(t *testing.T) {
	h := newHarness()
	h.aws.images["web:1.4.2"] = []ecrtypes.ImageDetail{
		{ImageDigest: aws.String(webDigest), ImageTags: []string{"1.4.2"}},
		{ImageDigest: aws.String(webDigest), ImageTags: []string{"1.4.2", "latest"}},
	}
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))
	if out.State != "succeeded" || serviceOf(t, m, "web").ImageDigest != webDigest {
		t.Errorf("outcome %q, digest %q", out.State, serviceOf(t, m, "web").ImageDigest)
	}

	h.aws.images["web:1.4.2"] = []ecrtypes.ImageDetail{
		{ImageDigest: aws.String(webDigest)}, {ImageDigest: aws.String(apiDigest)},
	}
	out, m = h.run(t, claimed(t, "prod", defaultSelection()))
	if c, _ := check(m, "web", "image exists"); out.State != "failed" || c.Status != "failed" {
		t.Errorf("two different digests for one tag must fail: %q %+v", out.State, c)
	}
}

func TestNoConfigServiceResolves(t *testing.T) {
	h := newHarness()
	sel := engine.Selection{"all": {Version: "1.4.3", Services: map[string]string{"api": ""}}}
	out, m := h.run(t, claimed(t, "prod", sel))
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	if len(m.Services) != 1 || len(m.Services[0].Config) != 0 {
		t.Fatalf("services = %+v", m.Services)
	}
	var names []string
	for _, c := range m.Checks {
		names = append(names, c.Name+"="+c.Status)
	}
	want := "service exists=passed,image exists=passed,write permission=not_verified"
	if strings.Join(names, ",") != want {
		t.Errorf("checks = %v, want %s", names, want)
	}
	if h.aws.callCount("s3:GetObject") != 0 {
		t.Error("a service without config must not touch S3")
	}
}

func TestSameInputsGiveSameIdentity(t *testing.T) {
	h := newHarness()
	_, first := h.run(t, claimed(t, "prod", defaultSelection()))
	keys := len(h.artifacts.items)
	_, second := h.run(t, claimed(t, "prod", defaultSelection()))
	if first.ID == "" || first.ID != second.ID {
		t.Errorf("identity %q then %q", first.ID, second.ID)
	}
	if len(h.artifacts.items) != keys {
		t.Errorf("rerun wrote %d artifact keys, want the same %d overwritten", len(h.artifacts.items), keys)
	}
	if !bytes.Equal(mustJSON(t, first), mustJSON(t, second)) {
		t.Error("the manifests differ between runs of the same inputs")
	}

	// A moved tag is a different manifest.
	h.aws.images["web:1.4.2"] = []ecrtypes.ImageDetail{{ImageDigest: aws.String(apiDigest), ImageTags: []string{"1.4.2"}}}
	_, third := h.run(t, claimed(t, "prod", defaultSelection()))
	if third.ID == first.ID {
		t.Error("a different digest must change the identity")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLogNeverContainsFileContents(t *testing.T) {
	h := newHarness()
	out, _ := h.run(t, claimed(t, "prod", defaultSelection()))
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q", out.State)
	}
	logged := h.env.text()
	if logged == "" {
		t.Fatal("nothing was logged")
	}
	for _, secret := range []string{secretEnvValue, secretTomlValue, secretPrevEnvValue, secretPrevToml, "hunter2"} {
		if strings.Contains(logged, secret) {
			t.Errorf("log contains a file value %q", secret)
		}
		if strings.Contains(string(out.Result), secret) {
			t.Errorf("result contains a file value %q", secret)
		}
	}
	// Files are named with their size and the first 12 characters of the checksum.
	if want := sha(envContent)[:12]; !strings.Contains(logged, want) || strings.Contains(logged, sha(envContent)[:13]) {
		t.Errorf("log should carry exactly the first 12 characters %q of the env checksum:\n%s", want, logged)
	}
	if !strings.Contains(logged, "web.env") {
		t.Error("log should name the file")
	}
}

func TestGitHubFactoryErrorTextIsNeverLogged(t *testing.T) {
	const plantedToken = "ghp_plantedTokenValue123"
	h := newHarness()
	h.deps.GitHub = func(context.Context, string) (resolve.Source, error) {
		return nil, fmt.Errorf("bad credentials for %s", plantedToken)
	}
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))
	if out.State != "failed" {
		t.Fatalf("State = %q, want failed", out.State)
	}
	if c, _ := check(m, "web", "config source web.env"); c.Status != "failed" {
		t.Errorf("check = %+v, want failed when there is no GitHub access", c)
	}
	if !strings.Contains(h.env.text(), "GitHub access") {
		t.Errorf("the log should still say that GitHub access failed:\n%s", h.env.text())
	}
	for _, text := range []string{h.env.text(), string(out.Result), out.FailureShort, out.FailureDetail} {
		if strings.Contains(text, plantedToken) {
			t.Errorf("the factory's error text reached %q", text)
		}
	}
}

func TestArtifactStoreFailureIsFailedCheckWithoutContent(t *testing.T) {
	h := newHarness()
	h.artifacts.fail = fmt.Errorf("put: %s", secretEnvValue) // an error text that must not be echoed
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))
	if out.State != "failed" {
		t.Fatalf("State = %q, want failed", out.State)
	}
	if c, _ := check(m, "web", "config source web.env"); c.Status != "failed" {
		t.Errorf("check = %+v", c)
	}
	for _, text := range []string{h.env.text(), string(out.Result), out.FailureDetail} {
		if strings.Contains(text, secretEnvValue) {
			t.Error("the store's error text was echoed")
		}
	}
}

func TestCancelRequestedStopsBeforeAnyCall(t *testing.T) {
	h := newHarness()
	h.env.cancel = true
	out, _ := h.run(t, claimed(t, "prod", defaultSelection()))
	if out.State != "cancelled" {
		t.Fatalf("State = %q, want cancelled", out.State)
	}
	if len(h.aws.calls) != 0 || len(h.artifacts.items) != 0 {
		t.Errorf("a cancelled resolve touched AWS %v or the artifact store", h.aws.calls)
	}
}

// --- failures through the real SDK: error code, request id, retries --------

// awsFake answers every AWS call with one error, through the real SDK
// clients, so the retryer and the error types are the production ones.
type awsFake struct {
	mu       sync.Mutex
	failCode string
	requests map[string]int // by X-Amz-Target operation, "s3" for S3
	status   int
}

func (f *awsFake) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasPrefix(r.URL.Hostname(), "sts.") { // the sign-in check passes; the other calls fail
		xml := `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::` + account +
			`:role/yumdrake</Arn><UserId>AROAEXAMPLE</UserId><Account>` + account + `</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>REQ-STS</RequestId></ResponseMetadata></GetCallerIdentityResponse>`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(xml)), Request: r}, nil
	}
	op := "s3"
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		op = target[strings.LastIndex(target, ".")+1:]
	}
	f.mu.Lock()
	f.requests[op]++
	f.mu.Unlock()

	header := http.Header{}
	var body string
	if op == "s3" {
		header.Set("Content-Type", "application/xml")
		header.Set("X-Amz-Request-Id", "REQ-S3-0001")
		body = fmt.Sprintf(`<?xml version="1.0"?><Error><Code>%s</Code><Message>secret message text</Message><RequestId>REQ-S3-0001</RequestId></Error>`, f.failCode)
	} else {
		header.Set("Content-Type", "application/x-amz-json-1.1")
		header.Set("X-Amzn-Requestid", "REQ-"+op+"-0001")
		body = fmt.Sprintf(`{"__type":%q,"message":"secret message text"}`, f.failCode)
	}
	return &http.Response{StatusCode: f.status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func (f *awsFake) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[op]
}

type noWait struct{}

func (noWait) BackoffDelay(int, error) (time.Duration, error) { return 0, nil }

// failingHarness resolves with the real SDK clients, every call failing with code.
func failingHarness(code string, status int) (*harness, *awsFake) {
	h := newHarness()
	fake := &awsFake{failCode: code, status: status, requests: map[string]int{}}
	h.deps.Clients = nil
	h.deps.AWS = func(_ context.Context, _, region, _ string) (aws.Config, error) {
		return aws.Config{
			Region:      region,
			Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "not-a-real-key", ""),
			HTTPClient:  &http.Client{Transport: fake},
			Retryer: func() aws.Retryer {
				return retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 3; o.Backoff = noWait{} })
			},
		}, nil
	}
	return h, fake
}

func TestAccessDeniedIsFailedCheckWithCodeAndRequestID(t *testing.T) {
	h, fake := failingHarness("AccessDeniedException", http.StatusBadRequest)
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))

	if out.State != "failed" || !strings.HasPrefix(out.FailureShort, "preflight failed: ") {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	c, ok := check(m, "web", "service exists")
	if !ok || c.Status != "failed" || !strings.Contains(c.Detail, "AccessDeniedException") || !strings.Contains(c.Detail, "REQ-DescribeServices-0001") {
		t.Errorf("service check = %+v, want the AWS error code and request id", c)
	}
	if c, _ := check(m, "web", "image exists"); !strings.Contains(c.Detail, "AccessDeniedException") || !strings.Contains(c.Detail, "REQ-DescribeImages-0001") {
		t.Errorf("image check = %+v", c)
	}
	logged := h.env.text()
	if !strings.Contains(logged, "AccessDeniedException") || !strings.Contains(logged, "REQ-DescribeServices-0001") {
		t.Errorf("log lacks the error code or request id:\n%s", logged)
	}
	for _, text := range []string{logged, string(out.Result), out.FailureDetail} {
		if strings.Contains(text, "secret message text") || strings.Contains(text, "not-a-real-key") {
			t.Errorf("the AWS message or a credential leaked:\n%s", text)
		}
	}
	if n := fake.count("DescribeServices"); n != 2 {
		t.Errorf("DescribeServices calls = %d, want one per service (a denial is not retried)", n)
	}
	// The env file comes from GitHub, which was not denied; every copy that
	// would have come from S3 is absent.
	for key := range h.artifacts.items {
		if strings.Contains(key, "/toml/") || strings.Contains(key, "/previous/") {
			t.Errorf("unexpected stored copy %q although S3 reads were denied", key)
		}
	}
}

func TestThrottlingIsRetriedThenFails(t *testing.T) {
	h, fake := failingHarness("ThrottlingException", http.StatusBadRequest)
	sel := engine.Selection{"all": {Version: "1.4.3", Services: map[string]string{"api": ""}}}
	out, m := h.run(t, claimed(t, "prod", sel))

	if n := fake.count("DescribeServices"); n != 3 {
		t.Errorf("DescribeServices attempts = %d, want 3 (the SDK retries throttling, then gives up)", n)
	}
	if out.State != "failed" {
		t.Fatalf("State = %q, want failed", out.State)
	}
	c, ok := check(m, "api", "service exists")
	if !ok || c.Status != "failed" || !strings.Contains(c.Detail, "ThrottlingException") || !strings.Contains(c.Detail, "REQ-DescribeServices-0001") {
		t.Errorf("check = %+v, want the throttling code and request id", c)
	}
	if !strings.Contains(h.env.text(), "ThrottlingException") {
		t.Errorf("log lacks the throttling code:\n%s", h.env.text())
	}
}
