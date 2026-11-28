//go:build e2e && floci

package e2e

// End-to-end proof of real deployments against the Floci AWS emulator. Each
// test seeds the dev account (three services running 1.3.0, images for 1.4.2,
// config sources and destinations), starts one orchestrator and one or two
// runners in-process with the real copy-config and ecs-deploy steps (wired by
// runnerapp.RegisterExecutors, no stand-ins), and drives everything through the
// API. The runners reach Floci through a proxy that records every call and can
// cut, hold or refuse the ones a test chooses.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/rcss-labs/yamdrake/test/awstest"
)

const (
	// deployWait bounds a wait for a deployment: the stability window alone is
	// 15 seconds per rollout, and Floci starts every task in a container.
	deployWait = 5 * time.Minute
)

var (
	// What the destinations hold before the deployments: web.env (seeded), web.toml
	// and api.env made different from what will be written, so every deployment
	// writes three files.
	webTomlStale = []byte("token = \"e2e-secret-toml-token-stale\"\n")
	apiEnvStale  = []byte("PORT=7070\nDB_PASSWORD=e2e-secret-db-password\n")
)

// ---- views of the API's answers ----

type deployLeg struct {
	ID              int64  `json:"id"`
	Step            string `json:"step"`
	Kind            string `json:"kind"`
	Service         string `json:"service"`
	State           string `json:"state"`
	Attempt         int    `json:"attempt"`
	CancelRequested bool   `json:"cancel_requested"`
	FailureShort    string `json:"failure_short"`
	FailureDetail   string `json:"failure_detail"`
}

type deployRun struct {
	ID         int64       `json:"id"`
	Kind       string      `json:"kind"`
	State      string      `json:"state"`
	WaitReason string      `json:"wait_reason"`
	EndReason  string      `json:"end_reason"`
	RestoreOf  *int64      `json:"restore_of"`
	Locks      []string    `json:"locks"`
	Legs       []deployLeg `json:"legs"`
	Summary    struct {
		Counts   map[string]int `json:"counts"`
		Failures []struct {
			LegID   int64  `json:"leg_id"`
			Service string `json:"service"`
			Step    string `json:"step"`
			State   string `json:"state"`
			Short   string `json:"short"`
			Detail  string `json:"detail"`
		} `json:"failures"`
	} `json:"summary"`
}

func (r deployRun) leg(t *testing.T, step, service string) deployLeg {
	t.Helper()
	for _, l := range r.Legs {
		if l.Step == step && l.Service == service {
			return l
		}
	}
	t.Fatalf("run %d has no leg of step %q for %q", r.ID, step, service)
	return deployLeg{}
}

func (s *flociStack) deployRun(id int64) deployRun {
	s.t.Helper()
	var r deployRun
	s.api.call(http.MethodGet, fmt.Sprintf("/api/v1/runs/%d", id), nil, &r)
	return r
}

// waitDeploy waits for the run to be in one of the states.
func (s *flociStack) waitDeploy(id int64, states ...string) deployRun {
	s.t.Helper()
	var last deployRun
	eventually(s.t, deployWait, fmt.Sprintf("run %d to be %s", id, strings.Join(states, " or ")), func() bool {
		last = s.deployRun(id)
		return slices.Contains(states, last.State)
	})
	return last
}

// expectDeploy waits for the run to be final and requires the state want. A run
// that ends another way is reported with every leg's state and failure.
func (s *flociStack) expectDeploy(id int64, want string) deployRun {
	s.t.Helper()
	states := []string{"succeeded", "partially_succeeded", "failed", "cancelled", "invalidated", "rejected", "expired"}
	if want == "needs_reconciliation" {
		states = append(states, want) // not final: it is only the answer when it is the one asked for
	}
	got := s.waitDeploy(id, states...)
	if got.State != want {
		var b strings.Builder
		for _, l := range got.Legs {
			fmt.Fprintf(&b, "\n  %s %s/%s %s attempt %d: %s %s", l.Kind, l.Step, l.Service, l.State, l.Attempt, l.FailureShort, l.FailureDetail)
		}
		s.t.Fatalf("run %d is %s (%s), want %s; legs:%s\nlog:\n%s", id, got.State, got.EndReason, want, b.String(), strings.Join(s.logLines(id), "\n"))
	}
	return got
}

// logLines is every line of the run's log, in order.
func (s *flociStack) logLines(id int64) []string {
	s.t.Helper()
	var lines []string
	after := ""
	for {
		var page struct {
			Lines []struct {
				LegID int64  `json:"leg_id"`
				Level string `json:"level"`
				Text  string `json:"text"`
			} `json:"lines"`
			Next string `json:"next"`
		}
		path := fmt.Sprintf("/api/v1/runs/%d/log?limit=1000", id)
		if after != "" {
			path += "&after=" + after
		}
		s.api.call(http.MethodGet, path, nil, &page)
		for _, l := range page.Lines {
			lines = append(lines, fmt.Sprintf("[leg %d] %s: %s", l.LegID, l.Level, l.Text))
		}
		if len(page.Lines) == 0 || page.Next == "" {
			return lines
		}
		after = page.Next
	}
}

// legLog is the lines of one leg.
func (s *flociStack) legLog(id, legID int64) []string {
	prefix := fmt.Sprintf("[leg %d] ", legID)
	var out []string
	for _, l := range s.logLines(id) {
		if rest, ok := strings.CutPrefix(l, prefix); ok {
			out = append(out, rest)
		}
	}
	return out
}

func anyContains(lines []string, parts ...string) bool {
	return slices.ContainsFunc(lines, func(l string) bool {
		for _, p := range parts {
			if !strings.Contains(l, p) {
				return false
			}
		}
		return true
	})
}

// ---- AWS helpers ----

func (s *flociStack) ecs(e *seededEnv) *ecs.Client {
	return ecs.NewFromConfig(e.acct.AWSConfig(flociRegion))
}

// serviceTaskDefinition is the task definition the service is pointed at.
func (s *flociStack) serviceTaskDefinition(e *seededEnv, service string) string {
	s.t.Helper()
	out, err := s.ecs(e).DescribeServices(context.Background(), &ecs.DescribeServicesInput{
		Cluster: aws.String(e.services[service].cluster), Services: []string{service},
	})
	if err != nil || len(out.Services) != 1 {
		s.t.Fatalf("describe service %s: %v", service, err)
	}
	return aws.ToString(out.Services[0].TaskDefinition)
}

// revisions lists the ACTIVE revisions of the service's task definition family.
func (s *flociStack) revisions(e *seededEnv, service string) []string {
	s.t.Helper()
	out, err := s.ecs(e).ListTaskDefinitions(context.Background(), &ecs.ListTaskDefinitionsInput{FamilyPrefix: aws.String(service), Status: ecstypes.TaskDefinitionStatusActive})
	if err != nil {
		s.t.Fatalf("list task definitions of %s: %v", service, err)
	}
	var arns []string
	for _, arn := range out.TaskDefinitionArns {
		if strings.Contains(arn, ":task-definition/"+service+":") {
			arns = append(arns, arn)
		}
	}
	return arns
}

func (s *flociStack) taskDefinition(e *seededEnv, arn string) (ecstypes.TaskDefinition, map[string]string) {
	s.t.Helper()
	out, err := s.ecs(e).DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(arn), Include: []ecstypes.TaskDefinitionField{ecstypes.TaskDefinitionFieldTags},
	})
	if err != nil || out.TaskDefinition == nil {
		s.t.Fatalf("describe task definition %s: %v", arn, err)
	}
	tags := map[string]string{}
	for _, tag := range out.Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return *out.TaskDefinition, tags
}

// requireImageOf requires the service's task definition to run the image of
// the plan's digest.
func (s *flociStack) requireImageOf(e *seededEnv, service, arn, digest string) {
	s.t.Helper()
	td, _ := s.taskDefinition(e, arn)
	var images []string
	for _, c := range td.ContainerDefinitions {
		images = append(images, aws.ToString(c.Image))
	}
	want := e.ecrURI(service) + "@" + digest
	if !slices.Contains(images, want) {
		s.t.Errorf("%s runs images %v, want %s", service, images, want)
	}
}

// staleDestinations makes the destinations of web.toml and api.env differ from
// what a deployment of the planned version writes, so that every one of the
// three files is written.
func (s *flociStack) staleDestinations(e *seededEnv) {
	s.t.Helper()
	e.acct.PutObject(e.dstBucket, "web.toml", webTomlStale)
	e.acct.PutObject(e.dstBucket, "api.env", apiEnvStale)
}

func (s *flociStack) requireDestination(e *seededEnv, key string, want []byte) {
	s.t.Helper()
	got, ok := e.acct.Object(e.dstBucket, key)
	if !ok {
		s.t.Errorf("%s does not exist, want %d bytes", key, len(want))
		return
	}
	if string(got) != string(want) {
		s.t.Errorf("%s holds %d bytes (sha256 %s), want %d bytes (sha256 %s)", key, len(got), sha(got), len(want), sha(want))
	}
}

// requireNoSecretInRunLog fails when a log line holds a config value.
func (s *flociStack) requireNoSecretInRunLog(id int64) {
	s.t.Helper()
	for _, line := range s.logLines(id) {
		for _, secret := range secretValues {
			if strings.Contains(line, secret) {
				s.t.Errorf("a log line holds a config value: %q", line)
			}
		}
	}
}

// putsTo counts the PutObject calls the runners made to a destination key.
func (s *flociStack) putsTo(e *seededEnv, key string) int {
	n := 0
	for _, r := range s.runnerProxy.Requests() {
		if isPutTo(r, e.dstBucket, key) {
			n++
		}
	}
	return n
}

func isPutTo(r awstest.Request, bucket, key string) bool {
	return r.Service == "s3" && r.Operation == "PutObject" && strings.HasSuffix(r.Path, "/"+key) &&
		(strings.Contains(r.Path, "/"+bucket+"/") || strings.HasPrefix(r.Host, bucket+"."))
}

func (s *flociStack) countCalls(service, operation string) int {
	n := 0
	for _, r := range s.runnerProxy.Requests() {
		if r.Service == service && r.Operation == operation {
			n++
		}
	}
	return n
}

// post sends a bodyless POST to the API and decodes the answer.
func (s *flociStack) post(path string, out any) {
	s.t.Helper()
	s.api.call(http.MethodPost, path, nil, out)
}

func realStack(t *testing.T) *flociStack {
	t.Helper()
	s := startFloci(t, flociOpts{seed: []string{devEnv}, real: true})
	s.staleDestinations(s.envs[devEnv])
	return s
}

// ---- the tests ----

// TestDeployEndToEnd runs a real deployment of three services and checks what
// AWS holds afterwards, what the log says and that nothing is left locked.
func TestDeployEndToEnd(t *testing.T) {
	s := realStack(t)
	dev := s.envs[devEnv]
	id := s.createRun(devEnv, planVersion, true)
	done := s.expectDeploy(id, "succeeded")
	plan, _ := s.plan(id)

	for _, name := range planServices {
		svc := dev.services[name]
		now := s.serviceTaskDefinition(dev, name)
		if now == svc.taskDef {
			t.Errorf("%s still uses its old task definition %s", name, now)
		}
		if revs := s.revisions(dev, name); len(revs) != 2 {
			t.Errorf("%s has %d revisions %v, want the old one and exactly one new", name, len(revs), revs)
		}
		digest := plan.Services[plan.service(t, name)].ImageDigest
		s.requireImageOf(dev, name, now, digest)
		_, tags := s.taskDefinition(dev, now)
		for key, want := range map[string]string{"yumdrake:version": planVersion, "yumdrake:run": fmt.Sprint(id)} {
			if tags[key] != want {
				t.Errorf("%s task definition tag %s = %q, want %q", name, key, tags[key], want)
			}
		}
		if tags["yumdrake:leg"] == "" || tags["yumdrake:manifest"] == "" {
			t.Errorf("%s task definition tags %v lack the leg or manifest stamp", name, tags)
		}

		// The log of the service's rollout names what was registered and updated
		// and the result.
		roll := done.leg(t, "roll", name)
		lines := s.legLog(id, roll.ID)
		if !anyContains(lines, "ok: Registered "+now) {
			t.Errorf("%s: the roll log has no 'Registered %s' line:\n%s", name, now, strings.Join(lines, "\n"))
		}
		if !anyContains(lines, "Updated "+name+" to "+now) {
			t.Errorf("%s: the roll log has no 'Updated' line", name)
		}
		if !anyContains(lines, "ok: "+name+" runs "+now) {
			t.Errorf("%s: the roll log has no success line", name)
		}
	}
	s.requireDestination(dev, "web.env", webEnvNew)
	s.requireDestination(dev, "web.toml", webToml)
	s.requireDestination(dev, "api.env", apiEnv)
	for service, files := range map[string][]string{"web": {"web.env", "web.toml"}, "api": {"api.env"}} {
		lines := s.legLog(id, done.leg(t, "config", service).ID)
		for _, file := range files {
			if !anyContains(lines, "ok: "+file+": written to s3://"+dev.dstBucket+"/"+file) {
				t.Errorf("%s: the config log has no line for the write of %s:\n%s", service, file, strings.Join(lines, "\n"))
			}
		}
	}
	// Every call the steps made is in a log: three registers, three updates and
	// three writes, no more.
	for operation, want := range map[string]int{"RegisterTaskDefinition": 3, "UpdateService": 3} {
		if got := s.countCalls("ecs", operation); got != want {
			t.Errorf("the runners called %s %d times, want %d", operation, got, want)
		}
	}
	for _, file := range []string{"web.env", "web.toml", "api.env"} {
		if got := s.putsTo(dev, file); got != 1 {
			t.Errorf("%s was written %d times, want once", file, got)
		}
	}
	s.requireNoSecretInRunLog(id)
	if len(done.Locks) != 0 {
		t.Errorf("the finished run still holds locks %v", done.Locks)
	}
	if done.Summary.Counts["succeeded"] == 0 || len(done.Summary.Failures) != 0 {
		t.Errorf("summary counts %v failures %v, want successes and no failures", done.Summary.Counts, done.Summary.Failures)
	}
}

// TestRunnerKilledAfterRegisterAdoptsRevision kills a runner whose
// RegisterTaskDefinition went through but was never answered. The other runner
// takes the leg, finds the revision the dead one registered and adopts it
// instead of registering another.
func TestRunnerKilledAfterRegisterAdoptsRevision(t *testing.T) {
	s := startFloci(t, flociOpts{seed: []string{devEnv}, real: true, noRunner: true})
	dev := s.envs[devEnv]
	s.staleDestinations(dev)
	a := s.startRunner("runner-a")

	held, release := s.runnerProxy.HoldAfterForward("ecs", "RegisterTaskDefinition")
	t.Cleanup(release)
	id := s.createRun(devEnv, planVersion, true)
	var heldRequest awstest.Request
	select {
	case heldRequest = <-held:
	case <-time.After(deployWait):
		t.Fatalf("no RegisterTaskDefinition was held; run is %s", s.deployRun(id).State)
	}
	// The register is on record as intended and has changed ECS; only the answer
	// is missing. The runner dies now, taking its other legs with it.
	a.kill()
	s.startRunner("runner-b")

	done := s.expectDeploy(id, "succeeded")
	for _, name := range planServices {
		if revs := s.revisions(dev, name); len(revs) != 2 {
			t.Errorf("%s has %d revisions %v, want the old one and exactly one new", name, len(revs), revs)
		}
		now := s.serviceTaskDefinition(dev, name)
		if now == dev.services[name].taskDef {
			t.Errorf("%s was not deployed", name)
		}
	}
	// The family of the held call is the service whose revision the second runner
	// must adopt. The killed runner's other legs may have been cut at any point;
	// whatever they did is adopted or done once, which the revision count above
	// and the register count below check.
	var heldService string
	for _, name := range planServices {
		if strings.Contains(string(heldRequest.Body), `"family":"`+name+`"`) {
			heldService = name
		}
	}
	if got := s.countCalls("ecs", "RegisterTaskDefinition"); got != 3 {
		t.Errorf("the runners called RegisterTaskDefinition %d times, want 3 (one per service; the second runner adopts, it does not register again)", got)
	}
	if heldService == "" {
		t.Fatal("the held RegisterTaskDefinition names none of the services")
	}
	roll := done.leg(t, "roll", heldService)
	if roll.Attempt < 2 {
		t.Errorf("%s's roll is on attempt %d, want the second runner's attempt 2 or later", heldService, roll.Attempt)
	}
	if lines := s.legLog(id, roll.ID); !anyContains(lines, "adopting arn:aws:ecs:", "registered by an earlier attempt") {
		t.Errorf("%s's roll log does not say it adopted the revision of the earlier attempt:\n%s", heldService, strings.Join(lines, "\n"))
	}
	if len(done.Locks) != 0 {
		t.Errorf("the finished run still holds locks %v", done.Locks)
	}
}

// TestCrashingVersionFailsThatServiceOnly deploys a version whose image exits
// at once for one service. Only that service fails, with the reason and exit
// code; the others are deployed.
func TestCrashingVersionFailsThatServiceOnly(t *testing.T) {
	s := realStack(t)
	dev := s.envs[devEnv]
	crash := dev.services["api"]
	crashDigest := dev.acct.RepoCrashingImage(flociRegion, crash.repo, planVersion, "crash-"+awstest.RandomSuffix(), 3)
	id := s.createRun(devEnv, planVersion, true)
	done := s.expectDeploy(id, "partially_succeeded")
	plan, _ := s.plan(id)
	if got := plan.Services[plan.service(t, "api")].ImageDigest; got != crashDigest {
		t.Fatalf("the plan pins api to %s, want the crashing image %s", got, crashDigest)
	}

	if len(done.Summary.Failures) != 1 {
		t.Fatalf("failures = %+v, want exactly one", done.Summary.Failures)
	}
	f := done.Summary.Failures[0]
	if f.Service != "api" || f.Step != "roll" || f.State != "failed" || f.Short != "tasks keep stopping" || !strings.Contains(f.Detail, "exited with 3") {
		t.Errorf("failure = %+v, want api's roll failed with 'tasks keep stopping' and exit code 3", f)
	}
	if !anyContains(s.legLog(id, f.LegID), "exit 3") {
		t.Errorf("the failed leg's log does not name exit code 3:\n%s", strings.Join(s.legLog(id, f.LegID), "\n"))
	}
	for _, name := range []string{"web", "worker"} {
		if st := done.leg(t, "roll", name).State; st != "succeeded" {
			t.Errorf("%s roll is %s, want succeeded", name, st)
		}
		now := s.serviceTaskDefinition(dev, name)
		s.requireImageOf(dev, name, now, plan.Services[plan.service(t, name)].ImageDigest)
		if now == dev.services[name].taskDef {
			t.Errorf("%s was not deployed", name)
		}
	}
	if len(done.Locks) != 0 {
		t.Errorf("the finished run still holds locks %v", done.Locks)
	}
}

// TestConfigWriteDeniedHalfway refuses the second file of web's config copy.
// web's config leg fails saying what was and was not written, its rollout is
// skipped, and re-running the failed services writes only the missing file.
func TestConfigWriteDeniedHalfway(t *testing.T) {
	s := realStack(t)
	dev := s.envs[devEnv]
	s.runnerProxy.FailNth("s3", "PutObject", 1, func(r awstest.Request) bool { return isPutTo(r, dev.dstBucket, "web.toml") }, "AccessDenied", http.StatusForbidden)

	id := s.createRun(devEnv, planVersion, true)
	done := s.expectDeploy(id, "partially_succeeded")

	config := done.leg(t, "config", "web")
	if config.State != "failed" {
		t.Fatalf("web's config leg is %s, want failed", config.State)
	}
	for _, want := range []string{"Written: web.env.", "Not written: web.toml."} {
		if !strings.Contains(config.FailureDetail, want) {
			t.Errorf("web's config failure %q lacks %q", config.FailureDetail, want)
		}
	}
	if st := done.leg(t, "roll", "web").State; st != "skipped" {
		t.Errorf("web's roll is %s, want skipped", st)
	}
	for _, name := range []string{"api", "worker"} {
		if st := done.leg(t, "roll", name).State; st != "succeeded" {
			t.Errorf("%s roll is %s, want succeeded", name, st)
		}
	}
	if now := s.serviceTaskDefinition(dev, "web"); now != dev.services["web"].taskDef {
		t.Errorf("web was deployed (%s) although its config was not written", now)
	}
	s.requireDestination(dev, "web.env", webEnvNew)
	s.requireDestination(dev, "web.toml", webTomlStale)

	var rerun deployRun
	s.post(fmt.Sprintf("/api/v1/runs/%d/rerun-failed", id), &rerun)
	if rerun.ID == id || rerun.ID == 0 {
		t.Fatalf("re-run failed returned run %d", rerun.ID)
	}
	s.waitDeploy(rerun.ID, "planned")
	s.start(rerun.ID)
	redone := s.expectDeploy(rerun.ID, "succeeded")
	for _, step := range []string{"config", "roll"} {
		for _, l := range redone.Legs {
			if l.Kind == "step" && l.Step == step && l.Service != "web" {
				t.Errorf("the re-run holds a leg of %s/%s, want web only", l.Step, l.Service)
			}
		}
	}
	s.requireDestination(dev, "web.env", webEnvNew)
	s.requireDestination(dev, "web.toml", webToml)
	// web.env was written once, by the first run; web.toml twice: the refused write
	// and the one that completed it.
	if got := s.putsTo(dev, "web.env"); got != 1 {
		t.Errorf("web.env was written %d times, want once (the re-run writes only the missing file)", got)
	}
	if got := s.putsTo(dev, "web.toml"); got != 2 {
		t.Errorf("web.toml was put %d times, want 2 (refused, then written)", got)
	}
	if now := s.serviceTaskDefinition(dev, "web"); now == dev.services["web"].taskDef {
		t.Error("web was not deployed by the re-run")
	}
	s.requireNoSecretInRunLog(id)
	s.requireNoSecretInRunLog(rerun.ID)
}

// TestRestoreEndToEnd restores a finished deployment: the plan shows the way
// back, and afterwards every service uses its exact previous task definition
// and every destination holds its previous bytes.
func TestRestoreEndToEnd(t *testing.T) {
	s := realStack(t)
	dev := s.envs[devEnv]
	id := s.createRun(devEnv, planVersion, true)
	s.expectDeploy(id, "succeeded")

	var restore deployRun
	s.post(fmt.Sprintf("/api/v1/runs/%d/restore", id), &restore)
	if restore.Kind != "restore" || restore.RestoreOf == nil || *restore.RestoreOf != id {
		t.Fatalf("restore run = kind %q restore_of %v, want a restore of run %d", restore.Kind, restore.RestoreOf, id)
	}
	// The restore is planned by the runner like any run; it waits to be started.
	s.waitDeploy(restore.ID, "planned")
	plan, raw := s.plan(restore.ID)
	for _, name := range planServices {
		svc := plan.Services[plan.service(t, name)]
		if svc.Version != runningVersion || svc.PreviousVersion != planVersion {
			t.Errorf("%s restore plan goes from %q to %q, want %s to %s", name, svc.PreviousVersion, svc.Version, planVersion, runningVersion)
		}
	}
	configChanges := map[string]string{}
	for _, svc := range plan.Services {
		for _, c := range svc.Config {
			configChanges[svc.Name+"/"+c.Name] = c.Change
		}
	}
	for _, file := range []string{"web/web.env", "web/web.toml", "api/api.env"} {
		if configChanges[file] != "changed" {
			t.Errorf("the restore plan's change of %s is %q, want changed (all: %v)", file, configChanges[file], configChanges)
		}
	}
	for _, secret := range secretValues {
		if strings.Contains(raw, secret) {
			t.Errorf("the restore plan holds a config value %q", secret)
		}
	}

	s.start(restore.ID)
	done := s.expectDeploy(restore.ID, "succeeded")
	for _, name := range planServices {
		if now, want := s.serviceTaskDefinition(dev, name), dev.services[name].taskDef; now != want {
			t.Errorf("%s uses %s after the restore, want its previous task definition %s", name, now, want)
		}
	}
	s.requireDestination(dev, "web.env", webEnvOld)
	s.requireDestination(dev, "web.toml", webTomlStale)
	s.requireDestination(dev, "api.env", apiEnvStale)
	if len(done.Locks) != 0 {
		t.Errorf("the finished restore still holds locks %v", done.Locks)
	}
	s.requireNoSecretInRunLog(restore.ID)
}

// TestCancelDuringRollout cancels while the first service rolls. The services
// that had not started are cancelled; the one in flight finishes and says what
// it did.
func TestCancelDuringRollout(t *testing.T) {
	s := startFloci(t, flociOpts{seed: []string{devEnv}, real: true, rollMaxParallel: 1})
	dev := s.envs[devEnv]
	s.staleDestinations(dev)
	id := s.createRun(devEnv, planVersion, true)

	var rolling deployLeg
	eventually(t, deployWait, "one service to be rolling", func() bool {
		for _, l := range s.deployRun(id).Legs {
			if l.Step == "roll" && l.State == "running" {
				rolling = l
				return true
			}
		}
		return false
	})
	// Cancel only once the rolling service has been updated: before that the leg
	// may still end "cancelled before the deployment started", which is another case.
	eventually(t, deployWait, "the rolling service to be updated", func() bool {
		return anyContains(s.legLog(id, rolling.ID), "Updated "+rolling.Service+" to ")
	})
	s.post(fmt.Sprintf("/api/v1/runs/%d/cancel", id), nil)
	done := s.expectDeploy(id, "cancelled")

	byState := map[string][]string{}
	for _, name := range planServices {
		l := done.leg(t, "roll", name)
		byState[l.State] = append(byState[l.State], name)
	}
	if got := done.leg(t, "roll", rolling.Service); got.State != "succeeded" {
		t.Errorf("the rolling service %s ended %s (%s), want succeeded: a started rollout is never abandoned", got.Service, got.State, got.FailureShort)
	}
	if len(byState["succeeded"]) != 1 || len(byState["cancelled"]) != 2 {
		t.Errorf("roll legs by state = %v, want one succeeded and two cancelled", byState)
	}
	for _, name := range byState["cancelled"] {
		if now := s.serviceTaskDefinition(dev, name); now != dev.services[name].taskDef {
			t.Errorf("%s was cancelled but now uses %s", name, now)
		}
		if revs := s.revisions(dev, name); len(revs) != 1 {
			t.Errorf("%s was cancelled but has %d revisions %v", name, len(revs), revs)
		}
	}
	if now := s.serviceTaskDefinition(dev, rolling.Service); now == dev.services[rolling.Service].taskDef {
		t.Errorf("%s reports succeeded but is not deployed", rolling.Service)
	}
	if !anyContains(s.legLog(id, rolling.ID), "a cancel was requested") {
		t.Errorf("the rolling leg's log does not say that it saw the cancel:\n%s", strings.Join(s.legLog(id, rolling.ID), "\n"))
	}
	// What changed is what the run page lists: the leg states and counts.
	if done.Summary.Counts["succeeded"] == 0 || done.Summary.Counts["cancelled"] != 2 {
		t.Errorf("summary counts = %v, want succeeded legs and exactly two cancelled", done.Summary.Counts)
	}
	if len(done.Locks) != 0 {
		t.Errorf("the cancelled run still holds locks %v", done.Locks)
	}
}

// TestSecondRunOnSameServiceWaits starts a second run on the services of a
// running one. It stays queued, holding nothing, until the first is final.
func TestSecondRunOnSameServiceWaits(t *testing.T) {
	s := realStack(t)
	first := s.createRun(devEnv, planVersion, true)
	s.waitDeploy(first, "running")
	second := s.createRun(devEnv, planVersion, true)

	waitReason := fmt.Sprintf("waiting for run #%d", first)
	eventually(t, deployWait, "the second run to say whom it waits for", func() bool { return s.deployRun(second).WaitReason == waitReason })
	for range 10 {
		time.Sleep(300 * time.Millisecond)
		a, b := s.deployRun(first), s.deployRun(second)
		if a.State != "running" {
			break // the first run is done; the second may go on
		}
		if b.State != "queued" || len(b.Locks) != 0 {
			t.Fatalf("while run %d runs, run %d is %s with locks %v, want queued with none", first, second, b.State, b.Locks)
		}
		for _, l := range b.Legs {
			if l.Kind == "step" && l.State != "pending" {
				t.Fatalf("the waiting run's %s leg %s/%s is %s, want pending", l.Kind, l.Step, l.Service, l.State)
			}
		}
	}
	a := s.expectDeploy(first, "succeeded")
	if len(a.Locks) != 0 {
		t.Errorf("the first run still holds locks %v", a.Locks)
	}
	// The second run was planned before the first changed the services, so what it
	// planned from is stale (it still names the old task definitions): when it gets
	// its locks, the re-check finds the services changed and runs nothing.
	b := s.expectDeploy(second, "invalidated")
	for _, l := range b.Legs {
		if l.Kind == "step" && l.State != "pending" && l.State != "skipped" && l.State != "cancelled" {
			t.Errorf("the invalidated second run's leg %s/%s is %s, want it never to have run", l.Step, l.Service, l.State)
		}
	}
	if len(b.Locks) != 0 {
		t.Errorf("the invalidated second run still holds locks %v", b.Locks)
	}
}

// TestCheckAgainAfterUnknownFinishes cuts the answer to one UpdateService, so
// nobody knows whether it went through. The run waits for a person; checking
// again finds the service already on the revision and the run succeeds.
func TestCheckAgainAfterUnknownFinishes(t *testing.T) {
	s := realStack(t)
	dev := s.envs[devEnv]
	// The SDK retries a cut connection, so every try for web's update is cut.
	s.runnerProxy.DropAfterForwardWhere("ecs", "UpdateService", 5, func(r awstest.Request) bool {
		return strings.Contains(string(r.Body), `"service":"web"`) || strings.Contains(string(r.Body), `"service": "web"`)
	})
	id := s.createRun(devEnv, planVersion, true)
	held := s.expectDeploy(id, "needs_reconciliation")
	// Drop rules the retries did not use must not cut the check that follows.
	s.runnerProxy.DisarmDrops()

	unknown := held.leg(t, "roll", "web")
	if unknown.State != "unknown" {
		t.Fatalf("web's roll is %s, want unknown", unknown.State)
	}
	for _, name := range []string{"api", "worker"} {
		if st := held.leg(t, "roll", name).State; st != "succeeded" {
			t.Errorf("%s roll is %s, want succeeded", name, st)
		}
	}
	if len(held.Locks) == 0 {
		t.Error("the run waits for a person but holds no locks")
	}
	if now := s.serviceTaskDefinition(dev, "web"); now == dev.services["web"].taskDef {
		t.Fatal("the dropped UpdateService did not reach ECS: the test would prove nothing")
	}

	s.post(fmt.Sprintf("/api/v1/runs/%d/legs/%d/check-again", id, unknown.ID), nil)
	done := s.expectDeploy(id, "succeeded")
	if len(done.Locks) != 0 {
		t.Errorf("the finished run still holds locks %v", done.Locks)
	}
	if revs := s.revisions(dev, "web"); len(revs) != 2 {
		t.Errorf("web has %d revisions %v, want the old one and exactly one new", len(revs), revs)
	}
	if !anyContains(s.legLog(id, unknown.ID), "An earlier attempt's update of the service is in place") {
		t.Errorf("web's log does not say the earlier update was found in place:\n%s", strings.Join(s.legLog(id, unknown.ID), "\n"))
	}
}
