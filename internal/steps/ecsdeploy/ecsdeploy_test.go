package ecsdeploy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner/runnertest"
	"github.com/rcss-labs/yamdrake/internal/steps/ecsdeploy"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const (
	account    = "123456789012"
	region     = "eu-west-1"
	serviceARN = "arn:aws:ecs:" + region + ":" + account + ":service/c/web"
	repoURI    = account + ".dkr.ecr." + region + ".amazonaws.com/web"
	digest     = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	runID      = 5
	legID      = 77
)

var t0 = time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

func tdARN(rev int) string {
	return fmt.Sprintf("arn:aws:ecs:%s:%s:task-definition/web:%d", region, account, rev)
}

// refusal is what the SDK returns for an HTTP answer with the given status.
func refusal(status int, code string) error {
	return &awshttp.ResponseError{RequestID: "req-" + code, ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      &smithy.GenericAPIError{Code: code, Message: "secret detail"},
	}}
}

// fakeClock is virtual time: Sleep moves it forward.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return nil
}

type fakeTD struct {
	td   ecstypes.TaskDefinition
	tags []ecstypes.Tag
}

// fakeECS is an in-memory ECS with one service.
type fakeECS struct {
	mu  sync.Mutex
	tds []*fakeTD
	svc ecstypes.Service

	updated       bool
	updatedARN    string
	polls         int  // DescribeServices calls since the update
	completeAfter int  // polls after which the rollout completes
	stall         bool // never complete
	onPoll        func(n int, f *fakeECS) error
	events        []ecstypes.ServiceEvent
	stopped       []ecstypes.Task
	listTasksErr  func(poll int) error // ListTasks fails when it returns an error

	hideLists       int // the first N ListTaskDefinitions calls see nothing
	listCalls       int
	registerErr     error
	registerThenErr error // registers, then returns this
	updateErr       error
	updateThenErr   error

	registers []*ecs.RegisterTaskDefinitionInput
	updates   []*ecs.UpdateServiceInput
}

func newFakeECS() *fakeECS {
	f := &fakeECS{completeAfter: 2}
	f.addTD(7, nil)
	f.svc = ecstypes.Service{
		ServiceArn: aws.String(serviceARN), TaskDefinition: aws.String(tdARN(7)), DesiredCount: 2, RunningCount: 2,
		Deployments: []ecstypes.Deployment{{
			Status: aws.String("PRIMARY"), TaskDefinition: aws.String(tdARN(7)), RolloutState: ecstypes.DeploymentRolloutStateCompleted,
			DesiredCount: 2, RunningCount: 2, CreatedAt: aws.Time(t0.Add(-time.Hour)),
		}},
	}
	return f
}

// addTD stores a revision of family web; tags nil means the previous one's.
func (f *fakeECS) addTD(rev int32, tags []ecstypes.Tag) {
	f.tds = append(f.tds, &fakeTD{
		td: ecstypes.TaskDefinition{
			TaskDefinitionArn: aws.String(tdARN(int(rev))), Family: aws.String("web"), Revision: rev,
			Cpu: aws.String("256"), Memory: aws.String("512"),
			ContainerDefinitions: []ecstypes.ContainerDefinition{{Name: aws.String("app"), Image: aws.String(repoURI + ":old"), Essential: aws.Bool(true)}},
		},
		tags: tags,
	})
}

func stamp(run, leg int) []ecstypes.Tag {
	return []ecstypes.Tag{
		{Key: aws.String(ecsdeploy.TagRun), Value: aws.String(strconv.Itoa(run))},
		{Key: aws.String(ecsdeploy.TagLeg), Value: aws.String(strconv.Itoa(leg))},
	}
}

func (f *fakeECS) ListTaskDefinitions(_ context.Context, in *ecs.ListTaskDefinitionsInput, _ ...func(*ecs.Options)) (*ecs.ListTaskDefinitionsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	out := &ecs.ListTaskDefinitionsOutput{}
	if f.listCalls <= f.hideLists {
		return out, nil
	}
	for i := len(f.tds) - 1; i >= 0 && len(out.TaskDefinitionArns) < int(aws.ToInt32(in.MaxResults)); i-- {
		if strings.HasPrefix(aws.ToString(f.tds[i].td.Family), aws.ToString(in.FamilyPrefix)) {
			out.TaskDefinitionArns = append(out.TaskDefinitionArns, aws.ToString(f.tds[i].td.TaskDefinitionArn))
		}
	}
	return out, nil
}

func (f *fakeECS) DescribeTaskDefinition(_ context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.tds {
		if aws.ToString(d.td.TaskDefinitionArn) == aws.ToString(in.TaskDefinition) {
			out := &ecs.DescribeTaskDefinitionOutput{TaskDefinition: &d.td}
			if slices.Contains(in.Include, ecstypes.TaskDefinitionFieldTags) {
				out.Tags = d.tags
			}
			return out, nil
		}
	}
	return nil, refusal(400, "ClientException")
}

func (f *fakeECS) RegisterTaskDefinition(_ context.Context, in *ecs.RegisterTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registers = append(f.registers, in)
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	rev := f.tds[len(f.tds)-1].td.Revision + 1
	f.tds = append(f.tds, &fakeTD{td: ecstypes.TaskDefinition{
		TaskDefinitionArn: aws.String(tdARN(int(rev))), Family: in.Family, Revision: rev,
		ContainerDefinitions: in.ContainerDefinitions,
	}, tags: in.Tags})
	if f.registerThenErr != nil {
		return nil, f.registerThenErr
	}
	return &ecs.RegisterTaskDefinitionOutput{TaskDefinition: &f.tds[len(f.tds)-1].td}, nil
}

func (f *fakeECS) UpdateService(_ context.Context, in *ecs.UpdateServiceInput, _ ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, in)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	f.applyUpdate(aws.ToString(in.TaskDefinition))
	if f.updateThenErr != nil {
		return nil, f.updateThenErr
	}
	return &ecs.UpdateServiceOutput{Service: &f.svc}, nil
}

// applyUpdate is what ECS does: ours becomes PRIMARY, the old one ACTIVE.
func (f *fakeECS) applyUpdate(arn string) {
	f.updated, f.updatedARN = true, arn
	old := f.svc.Deployments[0]
	old.Status = aws.String("ACTIVE")
	f.svc.TaskDefinition = aws.String(arn)
	f.svc.Deployments = []ecstypes.Deployment{{
		Status: aws.String("PRIMARY"), TaskDefinition: aws.String(arn), RolloutState: ecstypes.DeploymentRolloutStateInProgress,
		DesiredCount: 2, RunningCount: 0, CreatedAt: aws.Time(t0),
	}, old}
}

func (f *fakeECS) complete() {
	f.svc.Deployments = []ecstypes.Deployment{{
		Status: aws.String("PRIMARY"), TaskDefinition: aws.String(f.updatedARN), RolloutState: ecstypes.DeploymentRolloutStateCompleted,
		DesiredCount: 2, RunningCount: 2, CreatedAt: aws.Time(t0),
	}}
	f.svc.RunningCount = 2
}

func (f *fakeECS) DescribeServices(_ context.Context, _ *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updated {
		f.polls++
		if f.onPoll != nil {
			if err := f.onPoll(f.polls, f); err != nil {
				return nil, err
			}
		}
		if !f.stall && f.polls >= f.completeAfter {
			f.complete()
		}
	}
	svc := f.svc
	svc.Events = nil
	for i := len(f.events) - 1; i >= 0; i-- { // newest first
		svc.Events = append(svc.Events, f.events[i])
	}
	return &ecs.DescribeServicesOutput{Services: []ecstypes.Service{svc}}, nil
}

func (f *fakeECS) ListTasks(_ context.Context, _ *ecs.ListTasksInput, _ ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listTasksErr != nil {
		if err := f.listTasksErr(f.polls); err != nil {
			return nil, err
		}
	}
	out := &ecs.ListTasksOutput{}
	for _, t := range f.stopped {
		out.TaskArns = append(out.TaskArns, aws.ToString(t.TaskArn))
	}
	return out, nil
}

func (f *fakeECS) DescribeTasks(_ context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ecs.DescribeTasksOutput{}
	for _, t := range f.stopped {
		if !slices.Contains(in.Tasks, aws.ToString(t.TaskArn)) {
			continue
		}
		if t.TaskDefinitionArn == nil {
			t.TaskDefinitionArn = aws.String(f.updatedARN)
		}
		out.Tasks = append(out.Tasks, t)
	}
	return out, nil
}

// crashedTask is a stopped task of "our" revision (filled in when described).
func crashedTask(id string, stoppedAt time.Time, code int32) ecstypes.Task {
	return ecstypes.Task{
		TaskArn:       aws.String("arn:aws:ecs:" + region + ":" + account + ":task/c/" + id),
		StoppedReason: aws.String("Essential container in task exited"), StoppedAt: aws.Time(stoppedAt),
		Containers: []ecstypes.Container{{Name: aws.String("app"), ExitCode: aws.Int32(code)}},
	}
}

type fixture struct {
	t      *testing.T
	ecs    *fakeECS
	env    *runnertest.Env
	clock  *fakeClock
	svc    manifest.Service
	task   queue.Claimed
	awsErr error // what the credentials call returns
	// timeout is the step's timeout; 0 leaves it to the executor's default.
	timeout time.Duration
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, ecs: newFakeECS(), env: &runnertest.Env{Attempt: 2}, clock: &fakeClock{t: t0}}
	f.svc = manifest.Service{
		Name: "web", Version: "1.4.0", ImageDigest: digest, ECSService: serviceARN, ECRRepository: repoURI,
		PreviousTaskDefinition: tdARN(7),
	}
	return f
}

func (f *fixture) run() queue.Outcome {
	f.t.Helper()
	raw, err := json.Marshal(manifest.Manifest{ID: "manifest-1", RunID: runID, AWSAccount: account, Services: []manifest.Service{f.svc}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.task = queue.Claimed{
		Leg: store.Leg{ID: legID, RunID: runID, Kind: "step", Service: "web", Attempt: f.env.Attempt},
		Run: store.Run{ID: runID, AWSAccount: account, Manifest: raw},
	}
	if f.timeout > 0 {
		f.task.Step = &store.RunStep{Timeout: f.timeout}
	}
	var sessions []string
	out := ecsdeploy.New(ecsdeploy.Deps{
		AWS: func(_ context.Context, acct, r, session string) (aws.Config, error) {
			if acct != account || r != region {
				f.t.Errorf("credentials asked for %s in %s", acct, r)
			}
			sessions = append(sessions, session)
			return aws.Config{}, f.awsErr
		},
		ECS:       func(aws.Config) ecsdeploy.ECSAPI { return f.ecs },
		Options:   awsx.Options{},
		PollEvery: 5 * time.Second, DefaultTimeout: 15 * time.Minute, Clock: f.clock,
	}).Execute(context.Background(), f.env, f.task)
	for _, s := range sessions {
		if s != "yumdrake-runner-run-5" {
			f.t.Errorf("session = %q", s)
		}
	}
	return out
}

func (f *fixture) result(out queue.Outcome) ecsdeploy.Result {
	f.t.Helper()
	var r ecsdeploy.Result
	if err := json.Unmarshal(out.Result, &r); err != nil {
		f.t.Fatalf("result %q: %v", out.Result, err)
	}
	return r
}

func (f *fixture) logText() string {
	var b strings.Builder
	for _, l := range f.env.Lines() {
		b.WriteString(l.Level + ": " + l.Text + "\n")
	}
	return b.String()
}

func (f *fixture) countLogged(substr string) int {
	n := 0
	for _, l := range f.env.Lines() {
		if strings.Contains(l.Text, substr) {
			n++
		}
	}
	return n
}

func (f *fixture) want(out queue.Outcome, state string) {
	f.t.Helper()
	if out.State != state {
		f.t.Fatalf("outcome = %q %q: %s\n%s", out.State, out.FailureShort, out.FailureDetail, f.logText())
	}
}

func tagValue(tags []ecstypes.Tag, key string) string {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == key {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

func opState(f *fixture, key string) string {
	for _, op := range f.env.OpsSnapshot() {
		if op.Key == key {
			return op.State
		}
	}
	return ""
}

func TestHappyPathRegistersUpdatesAndSucceeds(t *testing.T) {
	f := newFixture(t)
	out := f.run()
	f.want(out, "succeeded")

	if len(f.ecs.registers) != 1 || len(f.ecs.updates) != 1 {
		t.Fatalf("registers %d, updates %d", len(f.ecs.registers), len(f.ecs.updates))
	}
	reg := f.ecs.registers[0]
	if got := aws.ToString(reg.ContainerDefinitions[0].Image); got != repoURI+"@"+digest {
		t.Errorf("image = %q", got)
	}
	for key, want := range map[string]string{
		ecsdeploy.TagVersion: "1.4.0", ecsdeploy.TagRun: "5", ecsdeploy.TagLeg: "77", ecsdeploy.TagManifest: "manifest-1",
	} {
		if got := tagValue(reg.Tags, key); got != want {
			t.Errorf("tag %s = %q, want %q", key, got, want)
		}
	}
	newARN := tdARN(8)
	upd := f.ecs.updates[0]
	if aws.ToString(upd.TaskDefinition) != newARN || aws.ToString(upd.Cluster) != "c" || aws.ToString(upd.Service) != "web" {
		t.Errorf("update = %+v", upd)
	}
	if upd.ForceNewDeployment {
		t.Error("ForceNewDeployment must not be set")
	}
	res := f.result(out)
	if res.TaskDefinition != newARN || res.PreviousTaskDefinition != tdARN(7) || res.Adopted {
		t.Errorf("result = %+v", res)
	}
	if opState(f, "register-task-definition:77") != "done" || opState(f, "update-service:"+newARN) != "done" {
		t.Errorf("ops = %+v", f.env.OpsSnapshot())
	}
	if f.countLogged("web runs "+newARN+" with 2 of 2 tasks") != 1 {
		t.Errorf("log:\n%s", f.logText())
	}
}

func TestAdoptsTaggedRevisionInsteadOfRegisteringAgain(t *testing.T) {
	f := newFixture(t)
	f.ecs.addTD(8, stamp(runID, legID))   // an earlier attempt's registration
	f.ecs.addTD(9, stamp(runID, legID+1)) // another leg's, newer, must not be taken
	out := f.run()
	f.want(out, "succeeded")
	if len(f.ecs.registers) != 0 {
		t.Errorf("registered %d revisions, want 0", len(f.ecs.registers))
	}
	if len(f.ecs.updates) != 1 || aws.ToString(f.ecs.updates[0].TaskDefinition) != tdARN(8) {
		t.Errorf("updates = %+v", f.ecs.updates)
	}
	if res := f.result(out); !res.Adopted || res.TaskDefinition != tdARN(8) {
		t.Errorf("result = %+v", res)
	}
	if f.countLogged("adopting "+tdARN(8)+", registered by an earlier attempt") != 1 {
		t.Errorf("log:\n%s", f.logText())
	}
}

func TestAmbiguousRegisterLooksForTaggedRevisionFirst(t *testing.T) {
	intended := store.Op{ID: 1, Seq: 1, Name: "register-task-definition", Key: "register-task-definition:77", State: "intended"}

	t.Run("revision appears on the second look", func(t *testing.T) {
		f := newFixture(t)
		f.ecs.addTD(8, stamp(runID, legID))
		f.ecs.hideLists = 1 // the first lookup misses it
		f.env.PresetOps = []store.Op{intended}
		out := f.run()
		f.want(out, "succeeded")
		if len(f.ecs.registers) != 0 || !f.result(out).Adopted {
			t.Errorf("registers %d, result %+v", len(f.ecs.registers), f.result(out))
		}
	})
	t.Run("nothing found registers again under a new key", func(t *testing.T) {
		f := newFixture(t)
		f.env.PresetOps = []store.Op{intended}
		out := f.run()
		f.want(out, "succeeded")
		if len(f.ecs.registers) != 1 {
			t.Fatalf("registers = %d", len(f.ecs.registers))
		}
		if !slices.Contains(f.env.DoKeys(), "register-task-definition:77:retry-2") {
			t.Errorf("keys = %v", f.env.DoKeys())
		}
		if f.result(out).Adopted {
			t.Error("a fresh registration is not adopted")
		}
	})
}

func TestAmbiguousUpdateChecksServiceFirst(t *testing.T) {
	arn8 := tdARN(8)
	setup := func(t *testing.T) *fixture {
		f := newFixture(t)
		f.ecs.addTD(8, stamp(runID, legID))
		f.env.PresetOps = []store.Op{
			{ID: 1, Seq: 1, Name: "register-task-definition", Key: "register-task-definition:77", State: "done", Result: []byte(`{}`)},
			{ID: 2, Seq: 2, Name: "update-service", Key: "update-service:" + arn8, State: "intended"},
		}
		return f
	}
	t.Run("service already on it", func(t *testing.T) {
		f := setup(t)
		f.ecs.applyUpdate(arn8) // the earlier attempt's call went through
		out := f.run()
		f.want(out, "succeeded")
		if len(f.ecs.updates) != 0 {
			t.Errorf("updated again: %+v", f.ecs.updates)
		}
	})
	t.Run("service not on it", func(t *testing.T) {
		f := setup(t)
		out := f.run()
		f.want(out, "succeeded")
		if len(f.ecs.updates) != 1 || aws.ToString(f.ecs.updates[0].TaskDefinition) != arn8 {
			t.Fatalf("updates = %+v", f.ecs.updates)
		}
		if !slices.Contains(f.env.DoKeys(), "update-service:"+arn8+":retry-2") {
			t.Errorf("keys = %v", f.env.DoKeys())
		}
	})
}

func TestRestoreUsesTargetTaskDefinitionWithoutRegistering(t *testing.T) {
	f := newFixture(t)
	f.ecs.addTD(6, nil)
	f.svc.TargetTaskDefinition = tdARN(6)
	out := f.run()
	f.want(out, "succeeded")
	if len(f.ecs.registers) != 0 || f.ecs.listCalls != 0 {
		t.Errorf("registers %d, lists %d", len(f.ecs.registers), f.ecs.listCalls)
	}
	if len(f.ecs.updates) != 1 || aws.ToString(f.ecs.updates[0].TaskDefinition) != tdARN(6) {
		t.Errorf("updates = %+v", f.ecs.updates)
	}
	if res := f.result(out); res.TaskDefinition != tdARN(6) {
		t.Errorf("result = %+v", res)
	}
}

func TestAccessDeniedOnRegisterFailsWithCodeAndLeavesServiceAlone(t *testing.T) {
	f := newFixture(t)
	f.ecs.registerErr = refusal(400, "AccessDeniedException")
	out := f.run()
	f.want(out, "failed")
	if out.FailureShort != "AccessDeniedException on ecs:RegisterTaskDefinition" {
		t.Errorf("short = %q", out.FailureShort)
	}
	if !strings.Contains(out.FailureDetail, "req-AccessDeniedException") || strings.Contains(out.FailureDetail, "secret detail") {
		t.Errorf("detail = %q", out.FailureDetail)
	}
	if len(f.ecs.updates) != 0 {
		t.Errorf("the service was updated: %+v", f.ecs.updates)
	}
}

func TestRefusedUpdateFailsAtOnce(t *testing.T) {
	f := newFixture(t)
	f.ecs.updateErr = refusal(403, "AccessDeniedException")
	out := f.run()
	f.want(out, "failed")
	if out.FailureShort != "AccessDeniedException on ecs:UpdateService" {
		t.Errorf("short = %q", out.FailureShort)
	}
}

func TestLostReplyOnRegisterOrUpdateIsUnknown(t *testing.T) {
	f := newFixture(t)
	f.ecs.registerThenErr = errors.New("connection reset")
	out := f.run()
	f.want(out, "unknown")
	if len(f.ecs.updates) != 0 {
		t.Error("must not update after an unconfirmed register")
	}

	g := newFixture(t)
	g.ecs.updateThenErr = refusal(503, "ServiceUnavailable")
	out = g.run()
	g.want(out, "unknown")
	if g.result(out).TaskDefinition != tdARN(8) {
		t.Errorf("result = %s", out.Result)
	}
}

func TestEarlierRefusedUpdateStaysFailed(t *testing.T) {
	f := newFixture(t)
	f.ecs.addTD(8, stamp(runID, legID))
	f.env.PresetOps = []store.Op{{ID: 1, Seq: 1, Name: "update-service", Key: "update-service:" + tdARN(8), State: "failed"}}
	out := f.run()
	f.want(out, "failed")
	if out.FailureShort != "an earlier attempt's update was refused" || len(f.ecs.updates) != 0 {
		t.Errorf("short = %q, updates %d", out.FailureShort, len(f.ecs.updates))
	}
}

func TestFailureLogsStoppedTaskReasons(t *testing.T) {
	f := newFixture(t)
	f.ecs.stall = true
	for i := 1; i <= 6; i++ {
		f.ecs.stopped = append(f.ecs.stopped, crashedTask("t"+strconv.Itoa(i), t0.Add(time.Duration(i)*time.Second), 3))
	}
	out := f.run()
	f.want(out, "failed")
	if out.FailureShort != "tasks keep stopping" || out.FailureDetail != "Essential container in task exited; container app exited with 3" {
		t.Errorf("short %q detail %q", out.FailureShort, out.FailureDetail)
	}
	if n := f.countLogged("container app exit 3"); n != 5 {
		t.Errorf("%d stopped tasks logged, want 5\n%s", n, f.logText())
	}
	if f.countLogged("t6: Essential container in task exited; container app exit 3") != 1 || f.countLogged("t1:") != 0 {
		t.Errorf("want the five newest:\n%s", f.logText())
	}
}

func TestStoppedTasksFromBeforeTheUpdateAreIgnored(t *testing.T) {
	f := newFixture(t)
	for i := 1; i <= 3; i++ {
		f.ecs.stopped = append(f.ecs.stopped, crashedTask("old"+strconv.Itoa(i), t0.Add(-time.Hour), 3))
	}
	f.want(f.run(), "succeeded")
}

func TestTimeoutFailsWithCounts(t *testing.T) {
	f := newFixture(t)
	f.ecs.stall = true
	f.timeout = 10 * time.Minute
	out := f.run()
	f.want(out, "failed")
	if out.FailureShort != "timed out after 10m0s" || !strings.Contains(out.FailureDetail, "running 2 of 2 desired") {
		t.Errorf("short %q detail %q", out.FailureShort, out.FailureDetail)
	}

	// Without a step timeout the executor's default (15 minutes) applies.
	g := newFixture(t)
	g.ecs.stall = true
	out = g.run()
	if out.FailureShort != "timed out after 15m0s" {
		t.Errorf("short = %q", out.FailureShort)
	}
}

func TestCancelBeforeAnythingStartsIsCancelled(t *testing.T) {
	f := newFixture(t)
	f.env.Cancel = true
	out := f.run()
	f.want(out, "cancelled")
	if out.FailureShort != "cancelled before the deployment started" || len(f.ecs.registers)+len(f.ecs.updates) != 0 {
		t.Errorf("short %q, registers %d, updates %d", out.FailureShort, len(f.ecs.registers), len(f.ecs.updates))
	}
}

func TestCancelWithRegisteredRevisionDoesNotUpdate(t *testing.T) {
	f := newFixture(t)
	f.ecs.addTD(8, stamp(runID, legID))
	f.env.PresetOps = []store.Op{{ID: 1, Seq: 1, Name: "register-task-definition", Key: "register-task-definition:77", State: "intended"}}
	f.env.Cancel = true
	out := f.run()
	f.want(out, "failed")
	if out.FailureShort != "cancelled before the service was updated" || !strings.Contains(out.FailureDetail, tdARN(8)) {
		t.Errorf("short %q detail %q", out.FailureShort, out.FailureDetail)
	}
	if len(f.ecs.updates) != 0 || f.result(out).TaskDefinition != tdARN(8) {
		t.Errorf("updates %d, result %s", len(f.ecs.updates), out.Result)
	}
}

func TestCancelDuringRolloutKeepsObserving(t *testing.T) {
	f := newFixture(t)
	f.env.AfterDo = func(e *runnertest.Env) {
		if keys := e.DoKeys(); strings.HasPrefix(keys[len(keys)-1], "update-service:") {
			e.SetCancel(true)
		}
	}
	f.ecs.completeAfter = 4
	out := f.run()
	f.want(out, "succeeded")
	if f.ecs.polls < 4 {
		t.Errorf("stopped observing after %d polls", f.ecs.polls)
	}
	if n := f.countLogged("a cancel was requested; waiting for the rollout to finish so the outcome is known"); n != 1 {
		t.Errorf("cancel logged %d times\n%s", n, f.logText())
	}
}

func TestReadFailuresForTwoMinutesGiveUnknown(t *testing.T) {
	f := newFixture(t)
	f.ecs.stall = true
	f.ecs.onPoll = func(int, *fakeECS) error { return errors.New("connection refused") }
	out := f.run()
	f.want(out, "unknown")
	if out.FailureShort != "could not read the service for 2 minutes" {
		t.Errorf("short = %q", out.FailureShort)
	}
	if f.result(out).TaskDefinition != tdARN(8) {
		t.Errorf("result = %s", out.Result)
	}
}

func TestShortReadFailureDoesNotEndTheWait(t *testing.T) {
	f := newFixture(t)
	f.ecs.completeAfter = 8
	f.ecs.onPoll = func(n int, _ *fakeECS) error {
		if n >= 2 && n <= 4 {
			return errors.New("throttled")
		}
		return nil
	}
	f.want(f.run(), "succeeded")
}

func TestEachServiceEventIsLoggedOnce(t *testing.T) {
	f := newFixture(t)
	f.ecs.completeAfter = 5
	f.ecs.events = []ecstypes.ServiceEvent{{Id: aws.String("e0"), Message: aws.String("old news"), CreatedAt: aws.Time(t0.Add(-time.Hour))}}
	f.ecs.onPoll = func(n int, e *fakeECS) error {
		switch n {
		case 1:
			e.events = append(e.events, ecstypes.ServiceEvent{Id: aws.String("e1"), Message: aws.String("started 1 tasks"), CreatedAt: aws.Time(t0.Add(time.Second))})
		case 3:
			e.events = append(e.events, ecstypes.ServiceEvent{Id: aws.String("e2"), Message: aws.String("registered targets"), CreatedAt: aws.Time(t0.Add(2 * time.Second))})
		}
		return nil
	}
	f.want(f.run(), "succeeded")
	for msg, want := range map[string]int{"started 1 tasks": 1, "registered targets": 1, "old news": 0} {
		if n := f.countLogged(msg); n != want {
			t.Errorf("%q logged %d times, want %d\n%s", msg, n, want, f.logText())
		}
	}
}

func TestRolledBackByECSFails(t *testing.T) {
	f := newFixture(t)
	f.ecs.stall = true
	f.ecs.onPoll = func(n int, e *fakeECS) error {
		if n == 2 { // the circuit breaker puts the old revision back in charge
			e.svc.TaskDefinition = aws.String(tdARN(7))
			e.svc.Deployments = []ecstypes.Deployment{
				{Status: aws.String("PRIMARY"), TaskDefinition: aws.String(tdARN(7)), RolloutState: ecstypes.DeploymentRolloutStateCompleted, DesiredCount: 2, RunningCount: 2},
				{Status: aws.String("ACTIVE"), TaskDefinition: aws.String(e.updatedARN), RolloutState: ecstypes.DeploymentRolloutStateFailed, CreatedAt: aws.Time(t0)},
			}
		}
		return nil
	}
	out := f.run()
	f.want(out, "failed")
	if out.FailureShort != "ECS rolled back" || !strings.Contains(out.FailureDetail, "back on the previous revision") {
		t.Errorf("short %q detail %q", out.FailureShort, out.FailureDetail)
	}
}

func TestSuccessNeedsTheStableWindow(t *testing.T) {
	f := newFixture(t)
	f.ecs.completeAfter = 1 // the first poll already looks complete
	f.want(f.run(), "succeeded")
	// Polls are 5s apart on the virtual clock: success at the first poll, then
	// the 15s window must pass, so the fourth poll is the first that may end it.
	if f.ecs.polls != 4 {
		t.Errorf("ended after %d polls, want 4", f.ecs.polls)
	}
	if n := f.countLogged("rollout looks complete; confirming it stays stable for 15s"); n != 1 {
		t.Errorf("window announced %d times\n%s", n, f.logText())
	}
}

func TestBriefStabilityThenCrashFails(t *testing.T) {
	f := newFixture(t)
	f.ecs.completeAfter = 1
	f.ecs.onPoll = func(n int, e *fakeECS) error {
		switch n {
		case 2: // one task of ours stops inside the window: the window restarts
			e.stopped = append(e.stopped, crashedTask("a", t0.Add(time.Second), 3))
		case 3: // two more: three in all
			e.stopped = append(e.stopped, crashedTask("b", t0.Add(2*time.Second), 3), crashedTask("c", t0.Add(3*time.Second), 3))
		}
		return nil
	}
	out := f.run()
	f.want(out, "failed")
	if out.FailureShort != "tasks keep stopping" {
		t.Errorf("short = %q", out.FailureShort)
	}
}

func TestStoppedTaskInsideTheWindowRestartsIt(t *testing.T) {
	f := newFixture(t)
	f.ecs.completeAfter = 1
	f.ecs.onPoll = func(n int, e *fakeECS) error {
		if n == 3 { // t = 10s: one crash, fewer than the three that would fail the leg
			e.stopped = append(e.stopped, crashedTask("a", t0.Add(time.Second), 3))
		}
		return nil
	}
	f.want(f.run(), "succeeded")
	// The window restarts at poll 3 (10s) and ends at poll 6 (25s); without the
	// restart the leg would have ended at poll 4.
	if f.ecs.polls != 6 {
		t.Errorf("ended after %d polls, want 6", f.ecs.polls)
	}
}

func TestWindowDoesNotCompleteOverUnreadRounds(t *testing.T) {
	t.Run("stopped tasks could not be read", func(t *testing.T) {
		f := newFixture(t)
		f.ecs.completeAfter = 1
		f.ecs.listTasksErr = func(poll int) error {
			if poll >= 2 && poll <= 4 {
				return errors.New("throttled")
			}
			return nil
		}
		f.want(f.run(), "succeeded")
		// Good reads resume at poll 5 (20s): the window runs 20s..35s, poll 8.
		if f.ecs.polls != 8 {
			t.Errorf("ended after %d polls, want 8", f.ecs.polls)
		}
	})
	t.Run("the service could not be read", func(t *testing.T) {
		f := newFixture(t)
		f.ecs.completeAfter = 1
		f.ecs.onPoll = func(n int, _ *fakeECS) error {
			if n == 2 || n == 3 {
				return errors.New("throttled")
			}
			return nil
		}
		f.want(f.run(), "succeeded")
		// The first good read after the gap is poll 4 (15s); the window ends at poll 7.
		if f.ecs.polls != 7 {
			t.Errorf("ended after %d polls, want 7", f.ecs.polls)
		}
	})
}

// capPolls stops a wait that would otherwise never end: 400 polls is 2000s of
// virtual time, well past the 15 minute default timeout.
func capPolls(t *testing.T, then func(n int) error) func(int, *fakeECS) error {
	return func(n int, _ *fakeECS) error {
		if n > 400 {
			t.Fatalf("still waiting after %d polls", n)
		}
		if then != nil {
			return then(n)
		}
		return nil
	}
}

// wantUnconfirmedAtTimeout checks the leg ended unknown just after the 15 minute
// timeout: Elapsed first exceeds 900s at poll 182 (905s).
func wantUnconfirmedAtTimeout(t *testing.T, f *fixture, out queue.Outcome, detailWords string) {
	t.Helper()
	f.want(out, "unknown")
	if want := "rollout looked complete but could not be confirmed stable within 15m0s"; out.FailureShort != want {
		t.Errorf("short = %q, want %q", out.FailureShort, want)
	}
	if !strings.Contains(out.FailureDetail, detailWords) {
		t.Errorf("detail = %q, want it to mention %q", out.FailureDetail, detailWords)
	}
	if f.ecs.polls != 182 {
		t.Errorf("ended after %d polls, want 182", f.ecs.polls)
	}
}

func TestWindowEndsAtTimeoutWhenStoppedTasksCannotBeRead(t *testing.T) {
	f := newFixture(t)
	f.ecs.completeAfter = 1
	f.ecs.onPoll = capPolls(t, nil)
	f.ecs.listTasksErr = func(int) error { return refusal(400, "AccessDeniedException") }
	out := f.run()
	wantUnconfirmedAtTimeout(t, f, out, "stopped tasks could not be read")
	if !strings.Contains(out.FailureDetail, "AccessDeniedException") {
		t.Errorf("detail = %q, want the error code", out.FailureDetail)
	}
}

func TestWindowEndsAtTimeoutWhenServiceReadsFlap(t *testing.T) {
	f := newFixture(t)
	f.ecs.completeAfter = 1
	f.ecs.onPoll = capPolls(t, func(n int) error {
		if n%2 == 0 {
			return errors.New("throttled")
		}
		return nil
	})
	wantUnconfirmedAtTimeout(t, f, f.run(), "service could not be read")
}

func TestDesiredZeroNeedsNoStableWindow(t *testing.T) {
	f := newFixture(t)
	f.ecs.svc.DesiredCount = 0
	f.ecs.completeAfter = 100
	f.want(f.run(), "succeeded")
	if f.ecs.polls != 1 {
		t.Errorf("ended after %d polls, want 1", f.ecs.polls)
	}
}

func TestCredentialsFailureWithInheritedIntentIsUnknown(t *testing.T) {
	for _, name := range []string{"register-task-definition", "update-service"} {
		f := newFixture(t)
		f.awsErr = errors.New("sso expired")
		f.env.PresetOps = []store.Op{{ID: 1, Seq: 1, Name: name, Key: name + ":x", State: "intended"}}
		out := f.run()
		if out.State != "unknown" {
			t.Errorf("%s intended: outcome = %q %q, want unknown", name, out.State, out.FailureShort)
		}
	}
	// With nothing intended the same failure is a plain failure.
	f := newFixture(t)
	f.awsErr = errors.New("sso expired")
	f.want(f.run(), "failed")
}

func TestUpdateRecordedDoneButServiceIsOnAnotherRevisionFails(t *testing.T) {
	f := newFixture(t)
	f.ecs.addTD(8, stamp(runID, legID))
	f.ecs.updated, f.ecs.updatedARN = true, tdARN(8) // polling mode; the service itself is still on 7
	f.ecs.stall = true
	f.env.PresetOps = []store.Op{{ID: 1, Seq: 1, Name: "update-service", Key: "update-service:" + tdARN(8), State: "done", Result: []byte(`{}`)}}
	out := f.run()
	f.want(out, "failed")
	if out.FailureShort != "ECS rolled back" || len(f.ecs.updates) != 0 {
		t.Errorf("short %q, updates %d", out.FailureShort, len(f.ecs.updates))
	}
}
