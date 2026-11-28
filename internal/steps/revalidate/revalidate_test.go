package revalidate_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/steps/resolve"
	"github.com/rcss-labs/yamdrake/internal/steps/revalidate"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const (
	account     = "111122223333"
	runID       = 42
	webARN      = "arn:aws:ecs:eu-west-1:111122223333:service/main/web"
	webRepoURI  = "111122223333.dkr.ecr.eu-west-1.amazonaws.com/web"
	webTaskDef  = "arn:aws:ecs:eu-west-1:111122223333:task-definition/web:5"
	webTaskDef6 = "arn:aws:ecs:eu-west-1:111122223333:task-definition/web:6"
	webDigest   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	destEnvURI  = "s3://config-dest/env/web.env"
	destTomlURI = "s3://config-dest/toml/web.toml"
)

// Values that must never reach a log line or an outcome.
const (
	secretPrevEnv  = "old-env-secret-111"       //nolint:gosec // fixture value, not a credential
	secretOtherEnv = "hand-edited-secret-333"   //nolint:gosec // fixture value, not a credential
	secretToml     = "toml-secret-new-value"    //nolint:gosec // fixture value, not a credential
	secretAWSText  = "arn:aws:iam::1:role/leak" //nolint:gosec // fixture value, not a credential
)

var (
	envNew     = []byte("API_KEY=new-env-secret-222\nSHARED=same\n")
	envPrev    = []byte("API_KEY=" + secretPrevEnv + "\nSHARED=same\n")
	envChanged = []byte("API_KEY=" + secretOtherEnv + "\n")
	tomlNew    = []byte("password = \"" + secretToml + "\"\n")
)

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- stub AWS ---------------------------------------------------------------

type stubAWS struct {
	mu          sync.Mutex
	taskDef     string          // the task definition the service runs
	serviceGone bool            // DescribeServices reports it missing
	serviceName string          // when set, DescribeServices returns only this service, under this name
	images      map[string]bool // digests that exist
	objects     map[string][]byte
	s3Err       error   // returned by every GetObject when set
	serviceARN  string  // when set, the ARN DescribeServices reports for the service
	ecsErrors   []error // returned, one per call, by DescribeServices before it answers
	calls       []string
}

func (s *stubAWS) record(call string) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
}

func (s *stubAWS) DescribeServices(_ context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	s.record("ecs:DescribeServices")
	s.mu.Lock()
	if len(s.ecsErrors) > 0 {
		err := s.ecsErrors[0]
		s.ecsErrors = s.ecsErrors[1:]
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	if s.serviceGone {
		return &ecs.DescribeServicesOutput{Failures: []ecstypes.Failure{{Arn: aws.String(in.Services[0]), Reason: aws.String("MISSING")}}}, nil
	}
	svc := ecstypes.Service{
		ServiceArn: aws.String(webARN), ServiceName: aws.String("web"), Status: aws.String("ACTIVE"), TaskDefinition: aws.String(s.taskDef),
	}
	if s.serviceARN != "" {
		svc.ServiceArn = aws.String(s.serviceARN)
	}
	if s.serviceName != "" {
		svc.ServiceName = aws.String(s.serviceName)
		svc.ServiceArn = aws.String("arn:aws:ecs:eu-west-1:111122223333:service/main/" + s.serviceName)
	}
	return &ecs.DescribeServicesOutput{Services: []ecstypes.Service{svc}}, nil
}

func (s *stubAWS) DescribeTaskDefinition(context.Context, *ecs.DescribeTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	s.record("ecs:DescribeTaskDefinition")
	return nil, errors.New("revalidate has no need of task definition contents")
}

func (s *stubAWS) DescribeImages(_ context.Context, in *ecr.DescribeImagesInput, _ ...func(*ecr.Options)) (*ecr.DescribeImagesOutput, error) {
	s.record("ecr:DescribeImages")
	digest := aws.ToString(in.ImageIds[0].ImageDigest)
	if digest == "" || !s.images[digest] {
		return nil, &ecrtypes.ImageNotFoundException{Message: aws.String("not found")}
	}
	return &ecr.DescribeImagesOutput{ImageDetails: []ecrtypes.ImageDetail{{ImageDigest: aws.String(digest)}}}, nil
}

func (s *stubAWS) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	s.record("s3:GetObject")
	if s.s3Err != nil {
		return nil, s.s3Err
	}
	data, ok := s.objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{Message: aws.String("no such key")}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data)), ContentLength: aws.Int64(int64(len(data)))}, nil
}

// stubArtifacts serves the manifest's stored copies and checks hashes like
// *artifacts.Store does. It records every key asked for.
type stubArtifacts struct {
	mu    sync.Mutex
	items map[string][]byte
	asked []string
}

func (a *stubArtifacts) Put(context.Context, int64, string, string, string, bool, []byte) (string, string, error) {
	return "", "", errors.New("revalidate must not write artifacts")
}

func (a *stubArtifacts) Get(_ context.Context, key, wantSHA256 string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, key)
	data, ok := a.items[key]
	if !ok {
		return nil, fmt.Errorf("artifacts: get %s: %w", key, &s3types.NoSuchKey{Message: aws.String("no such key")})
	}
	if sha(data) != wantSHA256 {
		return nil, fmt.Errorf("%w: %s", artifacts.ErrCorrupt, key)
	}
	return data, nil
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
	panic("revalidate must not use Env.Do")
}

func (e *testEnv) text() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.lines, "\n")
}

// --- fixture ----------------------------------------------------------------

type harness struct {
	aws               *stubAWS
	artifacts         *stubArtifacts
	artifactsOverride resolve.ArtifactStore // used instead of artifacts when set
	env               *testEnv
	manifest          manifest.Manifest
	raw               []byte // overrides the encoded manifest when set
}

// putOnlyArtifacts is a store that can write but cannot read stored copies.
type putOnlyArtifacts struct{}

func (putOnlyArtifacts) Put(context.Context, int64, string, string, string, bool, []byte) (string, string, error) {
	return "", "", errors.New("put only")
}

// newHarness is a run planned for one service, "web", with a stored env file
// whose destination held envPrev, and a TOML whose destination was absent;
// the world still looks exactly like the plan.
func newHarness() *harness {
	envItem := manifest.ConfigItem{
		Kind: "env", Name: "web.env", Source: "acme/config/web.env", Destination: destEnvURI,
		SHA256: sha(envNew), ArtifactKey: "manifests/42/web/env/web.env", Size: int64(len(envNew)),
		PreviousExists: true, PreviousSHA256: sha(envPrev), PreviousArtifactKey: "manifests/42/web/env/previous/web.env", Change: "changed",
	}
	tomlItem := manifest.ConfigItem{
		Kind: "toml", Name: "web.toml", Source: "s3://config-source/web.toml", Destination: destTomlURI,
		SHA256: sha(tomlNew), ArtifactKey: "manifests/42/web/toml/web.toml", Size: int64(len(tomlNew)), Change: "new",
	}
	m := manifest.Manifest{
		RunID: runID, Environment: "prod-1", Label: "prod", AWSAccount: account,
		Services: []manifest.Service{{
			Name: "web", Group: "all", Version: "1.4.2", ImageDigest: webDigest,
			ECSService: webARN, ECRRepository: webRepoURI, PreviousTaskDefinition: webTaskDef,
			Config: []manifest.ConfigItem{envItem, tomlItem},
		}},
		Checks: []manifest.Check{},
	}
	m.ID = manifest.Identity(m)
	return &harness{
		aws: &stubAWS{
			taskDef: webTaskDef,
			images:  map[string]bool{webDigest: true},
			objects: map[string][]byte{"config-dest/env/web.env": envPrev},
		},
		artifacts: &stubArtifacts{items: map[string][]byte{
			envItem.ArtifactKey: envNew, envItem.PreviousArtifactKey: envPrev, tomlItem.ArtifactKey: tomlNew,
		}},
		env:      &testEnv{},
		manifest: m,
	}
}

func (h *harness) run(t *testing.T) queue.Outcome {
	t.Helper()
	raw := h.raw
	if raw == nil {
		var err error
		if raw, err = json.Marshal(h.manifest); err != nil {
			t.Fatal(err)
		}
	}
	var sessions []string
	deps := resolve.Deps{
		AWS: func(_ context.Context, accountID, region, session string) (aws.Config, error) {
			if accountID != account {
				return aws.Config{}, fmt.Errorf("unexpected account %s", accountID)
			}
			sessions = append(sessions, session)
			return aws.Config{Region: region}, nil
		},
		Clients:   func(aws.Config) resolve.Clients { return resolve.Clients{ECS: h.aws, ECR: h.aws, S3: h.aws} },
		Artifacts: h.artifacts,
		RetryBase: time.Nanosecond,
	}
	if h.artifactsOverride != nil {
		deps.Artifacts = h.artifactsOverride
	}
	task := queue.Claimed{
		Leg: store.Leg{ID: 2, RunID: runID, Kind: "revalidate"},
		Run: store.Run{ID: runID, Environment: "prod-1", Label: "prod", AWSAccount: account, Manifest: raw, ManifestID: h.manifest.ID},
	}
	out := revalidate.New(deps).Execute(context.Background(), h.env, task)
	for _, s := range sessions {
		if s != "yumdrake-runner-run-42" {
			t.Errorf("session name = %q", s)
		}
	}
	return out
}

// wantDrift fails the test unless the leg failed as "changed since planning"
// with every fragment in its detail, and no secret anywhere.
func (h *harness) wantDrift(t *testing.T, out queue.Outcome, fragments ...string) {
	t.Helper()
	if out.State != "failed" || out.FailureShort != "changed since planning" {
		t.Fatalf("outcome = %q %q (%s), want failed \"changed since planning\"\nlog:\n%s", out.State, out.FailureShort, out.FailureDetail, h.env.text())
	}
	for _, f := range fragments {
		if !strings.Contains(out.FailureDetail, f) {
			t.Errorf("detail %q does not contain %q", out.FailureDetail, f)
		}
	}
	h.noLeaks(t, out)
}

func (h *harness) noLeaks(t *testing.T, out queue.Outcome) {
	t.Helper()
	all := h.env.text() + "\n" + out.FailureShort + "\n" + out.FailureDetail + "\n" + string(out.Result)
	for _, secret := range []string{secretPrevEnv, secretOtherEnv, secretToml, secretAWSText, "new-env-secret-222"} {
		if strings.Contains(all, secret) {
			t.Errorf("secret %q leaked into the log or the outcome", secret)
		}
	}
}

func short(sum string) string { return sum[:12] }

// --- tests ------------------------------------------------------------------

func TestRevalidatePassesWhenNothingChanged(t *testing.T) {
	h := newHarness()
	out := h.run(t)
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q: %s\nlog:\n%s", out.State, out.FailureShort, out.FailureDetail, h.env.text())
	}
	// Every service, image, destination and stored copy was actually looked at.
	for _, call := range []string{"ecs:DescribeServices", "ecr:DescribeImages", "s3:GetObject"} {
		if !contains(h.aws.calls, call) {
			t.Errorf("%s was never called; calls = %v", call, h.aws.calls)
		}
	}
	// Artifact keys come from the manifest only.
	want := []string{"manifests/42/web/env/previous/web.env", "manifests/42/web/env/web.env", "manifests/42/web/toml/web.toml"}
	got := append([]string(nil), h.artifacts.asked...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("artifact keys read = %v, want %v", got, want)
	}
	h.noLeaks(t, out)
}

func TestServiceRedeployedSincePlanningFails(t *testing.T) {
	h := newHarness()
	h.aws.taskDef = webTaskDef6
	out := h.run(t)
	h.wantDrift(t, out, "web", webTaskDef, webTaskDef6)
}

func TestServiceRemovedSincePlanningFails(t *testing.T) {
	h := newHarness()
	h.aws.serviceGone = true
	out := h.run(t)
	h.wantDrift(t, out, "web")
}

func TestOnlyServiceReturnedIsMatchedLikeResolve(t *testing.T) {
	h := newHarness()
	h.aws.serviceName = "web-alias"
	out := h.run(t)
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q: %s\nlog:\n%s", out.State, out.FailureShort, out.FailureDetail, h.env.text())
	}
	h.noLeaks(t, out)
}

func TestRevalidateRefusesAServiceInAnotherAccount(t *testing.T) {
	h := newHarness()
	h.aws.serviceARN = "arn:aws:ecs:eu-west-1:999999999999:service/main/web"
	out := h.run(t)
	if out.State != "failed" || out.FailureShort == "" {
		t.Fatalf("outcome = %q %q, want failed", out.State, out.FailureShort)
	}
	if !strings.Contains(out.FailureDetail, "999999999999") || !strings.Contains(out.FailureDetail, account) {
		t.Errorf("detail %q does not name both accounts", out.FailureDetail)
	}
	h.noLeaks(t, out)
}

func TestRepositoryInAnotherAccountIsAllowed(t *testing.T) {
	h := newHarness()
	h.manifest.Services[0].ECRRepository = "999999999999.dkr.ecr.eu-west-1.amazonaws.com/web"
	h.manifest.ID = manifest.Identity(h.manifest)
	out := h.run(t)
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q: %s\nlog:\n%s", out.State, out.FailureShort, out.FailureDetail, h.env.text())
	}
}

func TestRevalidateRetriesThrottlingThenSucceeds(t *testing.T) {
	h := newHarness()
	throttled := &smithy.GenericAPIError{Code: "ThrottlingException", Message: secretAWSText}
	h.aws.ecsErrors = []error{throttled, throttled}
	out := h.run(t)
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q: %s\nlog:\n%s", out.State, out.FailureShort, out.FailureDetail, h.env.text())
	}
	n := 0
	for _, call := range h.aws.calls {
		if call == "ecs:DescribeServices" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("DescribeServices was called %d times, want 3 (two throttled, then the answer)", n)
	}
	h.noLeaks(t, out)
}

func TestDestinationChangedSincePlanningFails(t *testing.T) {
	h := newHarness()
	h.aws.objects["config-dest/env/web.env"] = envChanged
	out := h.run(t)
	h.wantDrift(t, out, "web", "web.env", short(sha(envPrev)), short(sha(envChanged)))
	if strings.Contains(out.FailureDetail, sha(envPrev)) {
		t.Errorf("detail carries a full checksum: %q", out.FailureDetail)
	}
}

func TestDestinationCreatedSincePlanningFails(t *testing.T) {
	h := newHarness()
	h.aws.objects["config-dest/toml/web.toml"] = []byte("password = \"" + secretToml + "\"\n")
	out := h.run(t)
	h.wantDrift(t, out, "web", "web.toml", "did not exist")
}

func TestDestinationRemovedSincePlanningFails(t *testing.T) {
	h := newHarness()
	delete(h.aws.objects, "config-dest/env/web.env")
	out := h.run(t)
	h.wantDrift(t, out, "web", "web.env", short(sha(envPrev)))
}

func TestImageDeletedSincePlanningFails(t *testing.T) {
	h := newHarness()
	h.aws.images = map[string]bool{}
	out := h.run(t)
	h.wantDrift(t, out, "web", short(webDigest[len("sha256:"):]))
}

func TestCorruptArtifactFails(t *testing.T) {
	for _, name := range []string{"manifests/42/web/env/web.env", "manifests/42/web/env/previous/web.env"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.artifacts.items[name] = []byte("tampered\n")
			out := h.run(t)
			h.wantDrift(t, out, "web", "web.env", "stored copy")
		})
	}
}

func TestMissingArtifactFails(t *testing.T) {
	h := newHarness()
	delete(h.artifacts.items, "manifests/42/web/toml/web.toml")
	out := h.run(t)
	h.wantDrift(t, out, "web", "web.toml", "stored copy")
}

func TestAWSErrorFailsWithCodeOnly(t *testing.T) {
	h := newHarness()
	h.aws.s3Err = &smithy.GenericAPIError{Code: "AccessDenied", Message: secretAWSText}
	out := h.run(t)
	if out.State != "failed" || out.FailureShort == "" {
		t.Fatalf("outcome = %q %q, want failed", out.State, out.FailureShort)
	}
	if !strings.Contains(out.FailureDetail, "AccessDenied") || !strings.Contains(out.FailureDetail, "web") {
		t.Errorf("detail %q does not name the service and the AWS code", out.FailureDetail)
	}
	// An answer we could not get is not drift: do not tell the user it changed.
	if out.FailureShort == "changed since planning" || !strings.HasPrefix(out.FailureShort, "could not re-check the targets") {
		t.Errorf("short reason %q: an AWS failure must say it could not re-check, not that the targets changed", out.FailureShort)
	}
	h.noLeaks(t, out)
}

func TestMissingOrUnreadableManifestFails(t *testing.T) {
	for name, raw := range map[string][]byte{"none": nil, "not json": []byte("{" + secretToml), "empty object": []byte(`{}`)} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			if raw == nil {
				h.raw = []byte{}
			} else {
				h.raw = raw
			}
			out := h.run(t)
			if out.State != "failed" || out.FailureShort == "" || out.FailureShort == "changed since planning" {
				t.Fatalf("outcome = %q %q, want failed with a manifest reason", out.State, out.FailureShort)
			}
			h.noLeaks(t, out)
			if len(h.aws.calls) != 0 {
				t.Errorf("AWS was called without a manifest: %v", h.aws.calls)
			}
		})
	}
}

func TestEverythingChangedIsReportedTogether(t *testing.T) {
	h := newHarness()
	h.aws.taskDef = webTaskDef6
	h.aws.images = map[string]bool{}
	h.aws.objects["config-dest/env/web.env"] = envChanged
	out := h.run(t)
	h.wantDrift(t, out, webTaskDef6, "web.env", "image")
}

func TestArtifactStoreWithoutReaderCannotPass(t *testing.T) {
	h := newHarness()
	h.artifactsOverride = putOnlyArtifacts{}
	out := h.run(t)
	if out.State != "failed" || !strings.HasPrefix(out.FailureShort, "could not re-check the targets") {
		t.Fatalf("outcome = %q %q (%s), want failed \"could not re-check the targets\"", out.State, out.FailureShort, out.FailureDetail)
	}
	if !strings.Contains(out.FailureDetail, "the artifact store cannot read stored copies") {
		t.Errorf("detail %q does not say the store cannot read stored copies", out.FailureDetail)
	}
	h.noLeaks(t, out)
}

func TestCancelledBeforeAnythingIsRead(t *testing.T) {
	h := newHarness()
	h.env.cancel = true
	out := h.run(t)
	if out.State != "cancelled" || len(h.aws.calls) != 0 {
		t.Fatalf("outcome = %q, calls = %v", out.State, h.aws.calls)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// *artifacts.Store must satisfy the reader the executor looks for.
var _ revalidate.ArtifactReader = (*artifacts.S3Store)(nil)
