// Package ecsdeploy is the "ecs-deploy" executor: it puts one ECS service on
// the task definition a manifest calls for and waits until the rollout is
// judged.
//
// A deploy registers a new revision of the service's family (the previous
// revision with the pinned image and the run's tags) and updates the service to
// it. A restore (the manifest names a target revision) only updates. Both
// registering and updating go through Env.Do. After a crash the executor finds
// out what happened by asking ECS: a revision tagged with this run and leg is
// adopted instead of registering another, and a service already on the revision
// needs no second update.
//
// The wait is Judge applied to what each poll saw. A cancel request does not
// stop the wait once the service has been updated: the outcome must be the real
// one.
//
// IAM actions called: ecs:ListTaskDefinitions, ecs:DescribeTaskDefinition,
// ecs:RegisterTaskDefinition (with ecs:TagResource, which RegisterTaskDefinition
// needs when tags are passed), ecs:UpdateService, ecs:DescribeServices,
// ecs:ListTasks, ecs:DescribeTasks, and iam:PassRole for the task and execution
// roles of the registered definition.
//
// Log lines and outcomes name AWS failures by error code and request id; AWS
// error messages are not copied.
package ecsdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/steps/resolve"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// ECSAPI is the part of the ECS client the step uses. *ecs.Client satisfies it.
type ECSAPI interface {
	ListTaskDefinitions(ctx context.Context, in *ecs.ListTaskDefinitionsInput, opts ...func(*ecs.Options)) (*ecs.ListTaskDefinitionsOutput, error)
	DescribeTaskDefinition(ctx context.Context, in *ecs.DescribeTaskDefinitionInput, opts ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error)
	RegisterTaskDefinition(ctx context.Context, in *ecs.RegisterTaskDefinitionInput, opts ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error)
	UpdateService(ctx context.Context, in *ecs.UpdateServiceInput, opts ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error)
	DescribeServices(ctx context.Context, in *ecs.DescribeServicesInput, opts ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
	ListTasks(ctx context.Context, in *ecs.ListTasksInput, opts ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	DescribeTasks(ctx context.Context, in *ecs.DescribeTasksInput, opts ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
}

// Clock lets tests run the wait without real time.
type Clock interface {
	Now() time.Time
	// Sleep waits d or until ctx ends, and returns ctx's error in that case.
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Deps are what the executor needs from the outside.
type Deps struct {
	// AWS returns an SDK configuration with the deployment credentials of an
	// account. session names the assumed role session.
	AWS func(ctx context.Context, accountID, region, session string) (aws.Config, error)
	// ECS builds the ECS client; nil means ecs.NewFromConfig.
	ECS            func(cfg aws.Config) ECSAPI
	Options        awsx.Options
	PollEvery      time.Duration // default 5s
	DefaultTimeout time.Duration // default 15m
	// StableFor is how long a succeeded verdict must hold, with no new
	// stopped task of ours, before the leg reports success. Default 15s.
	StableFor time.Duration
	Clock     Clock // nil means real time
}

// Result is the leg's result whenever the revision is known.
type Result struct {
	TaskDefinition         string `json:"task_definition"`
	PreviousTaskDefinition string `json:"previous_task_definition"`
	Adopted                bool   `json:"adopted"`
}

const (
	registerOp        = "register-task-definition"
	updateOp          = "update-service"
	sessionPrefix     = "yumdrake-runner-run-"
	lookupRevisions   = 10
	stoppedTasksShown = 5
	maxTaskPages      = 10
)

type executor struct{ d Deps }

// New returns the "ecs-deploy" executor.
func New(d Deps) runner.Executor {
	if d.ECS == nil {
		d.ECS = func(cfg aws.Config) ECSAPI { return ecs.NewFromConfig(cfg) }
	}
	if d.PollEvery <= 0 {
		d.PollEvery = 5 * time.Second
	}
	if d.StableFor <= 0 {
		d.StableFor = 15 * time.Second
	}
	if d.DefaultTimeout <= 0 {
		d.DefaultTimeout = 15 * time.Minute
	}
	if d.Clock == nil {
		d.Clock = realClock{}
	}
	return &executor{d: d}
}

// leg is one execution of the step.
type leg struct {
	x       *executor
	ctx     context.Context
	env     runner.Env
	task    queue.Claimed
	service manifest.Service
	m       manifest.Manifest
	ref     awsx.ECSServiceRef
	client  ECSAPI
	ops     []store.Op
	timeout time.Duration

	res          Result
	cancelLogged bool
	// pending names the operations (by name) an earlier attempt left intended
	// and this attempt has not yet reconciled against ECS.
	pending     map[string]bool
	lastReadErr error
}

// Execute puts the service on its target task definition and judges the rollout.
func (x *executor) Execute(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome {
	r := &leg{x: x, ctx: ctx, env: env, task: task, timeout: x.d.DefaultTimeout}
	if task.Step != nil && task.Step.Timeout > 0 {
		r.timeout = task.Step.Timeout
	}

	found := json.Unmarshal(task.Run.Manifest, &r.m) == nil
	if found {
		found = false
		for _, s := range r.m.Services {
			if s.Name == task.Leg.Service {
				r.service, found = s, true
				break
			}
		}
	}
	if !found {
		env.Log("error", "The run's manifest has no entry for this service.")
		return r.end("failed", "the run's manifest has no entry for this service", "")
	}
	ref, err := awsx.ParseECSService(r.service.ECSService)
	if err != nil {
		env.Log("error", "The service identifier in the manifest is not usable.")
		return r.end("failed", "the service identifier in the manifest is not usable", "")
	}
	r.ref = ref
	r.res.PreviousTaskDefinition = r.service.PreviousTaskDefinition

	ops, err := env.Ops(ctx)
	if err != nil {
		env.Log("error", "The leg's earlier operations could not be read.")
		return r.end("unknown", "could not read the leg's earlier operations", resolve.ErrorDetail("reading operations", err))
	}
	r.ops = ops
	r.pending = map[string]bool{}
	for _, op := range ops {
		if op.State == "intended" {
			r.pending[op.Name] = true
		}
	}

	if len(ops) == 0 && env.CancelRequested(ctx) {
		env.Log("warn", "Cancelled before anything was changed.")
		return r.end("cancelled", "cancelled before the deployment started", "")
	}

	cfg, err := x.d.AWS(ctx, task.Run.AWSAccount, ref.Region, sessionPrefix+strconv.FormatInt(task.Run.ID, 10))
	if err != nil {
		env.Log("error", fmt.Sprintf("Could not get credentials for account %s.", task.Run.AWSAccount))
		return r.end("failed", "could not get credentials for account "+task.Run.AWSAccount, resolve.ErrorDetail("sign-in", err))
	}
	r.client = x.d.ECS(cfg)

	if out := r.settleTaskDefinition(); out != nil {
		return *out
	}
	if out := r.updateService(); out != nil {
		return *out
	}
	return r.wait()
}

// --- the task definition ------------------------------------------------------

// settleTaskDefinition decides which revision the service goes to and sets
// r.res.TaskDefinition; a non-nil outcome ends the leg.
func (r *leg) settleTaskDefinition() *queue.Outcome {
	if target := r.service.TargetTaskDefinition; target != "" {
		r.res.TaskDefinition = target
		r.env.Log("info", "Restoring the service to "+target+".")
		return r.checkpoint()
	}
	family := familyOf(r.service.PreviousTaskDefinition)
	if family == "" {
		r.env.Log("error", "The previous task definition in the manifest is not usable.")
		return r.stopped("failed", "the previous task definition in the manifest is not usable", "")
	}

	arn, err := r.findTagged(family)
	if err != nil {
		return r.lookupFailed(err, r.hasOp(registerOp))
	}
	if arn != "" {
		return r.adopt(arn)
	}
	delete(r.pending, registerOp) // looked, and no revision of ours exists: an intended register did not happen
	if r.env.CancelRequested(r.ctx) {
		if len(r.ops) == 0 {
			return r.stopped("cancelled", "cancelled before the deployment started", "")
		}
		return r.stopped("failed", "cancelled before the service was updated", "No task definition was registered for this leg.")
	}

	prev, err := r.client.DescribeTaskDefinition(r.ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(r.service.PreviousTaskDefinition), Include: []ecstypes.TaskDefinitionField{ecstypes.TaskDefinitionFieldTags},
	})
	if err != nil || prev.TaskDefinition == nil {
		code := codeOf(err)
		r.env.Log("error", fmt.Sprintf("The previous task definition could not be read (%s).", code))
		state := "failed"
		if r.ctx.Err() != nil {
			state = "unknown"
		}
		return r.stopped(state, code+" on ecs:DescribeTaskDefinition", resolve.ErrorDetail("ecs:DescribeTaskDefinition", err))
	}
	next, err := NextTaskDefinition(*prev.TaskDefinition, prev.Tags, r.service.ECRRepository, r.service.ImageDigest, Stamp{
		Version: r.service.Version, ManifestID: r.m.ID, RunID: r.task.Run.ID, LegID: r.task.Leg.ID,
	})
	if err != nil {
		r.env.Log("error", "The next task definition could not be built: "+err.Error())
		if errors.Is(err, ErrNoMatchingContainer) {
			return r.stopped("failed", "no container uses the service's repository", err.Error())
		}
		return r.stopped("failed", "the next task definition could not be built", err.Error())
	}

	key := registerOp + ":" + strconv.FormatInt(r.task.Leg.ID, 10)
	a := r.registerOnce(next, key)
	if errors.Is(a.err, runner.ErrAmbiguous) {
		// An earlier attempt intended a registration and never recorded it.
		arn, err := r.findTagged(family)
		if err != nil {
			return r.lookupFailed(err, true)
		}
		if arn != "" {
			return r.adopt(arn)
		}
		if r.env.CancelRequested(r.ctx) {
			return r.stopped("failed", "cancelled before the service was updated", "No task definition was registered for this leg.")
		}
		a = r.registerOnce(next, fmt.Sprintf("%s:retry-%d", key, r.task.Leg.Attempt))
	}
	if out := r.refused(a, "register", "RegisterTaskDefinition"); out != nil {
		return out
	}
	if a.arn == "" {
		r.env.Log("error", "The registered revision could not be read back from the operation record.")
		return r.stopped("unknown", "could not read the registered revision", "")
	}
	r.res.TaskDefinition = a.arn
	r.env.Log("ok", "Registered "+a.arn+".")
	return r.checkpoint()
}

func (r *leg) adopt(arn string) *queue.Outcome {
	delete(r.pending, registerOp)
	r.res.TaskDefinition, r.res.Adopted = arn, true
	r.env.Log("info", fmt.Sprintf("adopting %s, registered by an earlier attempt", arn))
	return r.checkpoint()
}

func (r *leg) checkpoint() *queue.Outcome {
	if err := r.env.Checkpoint(r.ctx, r.res); err != nil {
		r.env.Log("error", "Progress could not be saved.")
		return r.stopped("unknown", "could not save progress", resolve.ErrorDetail("saving progress", err))
	}
	return nil
}

func (r *leg) lookupFailed(err error, interrupted bool) *queue.Outcome {
	code := codeOf(err)
	r.env.Log("error", fmt.Sprintf("Task definition revisions could not be listed (%s).", code))
	detail := resolve.ErrorDetail("ecs:ListTaskDefinitions", err)
	if interrupted || r.ctx.Err() != nil {
		return r.stopped("unknown", "could not check an interrupted register", detail)
	}
	return r.stopped("failed", code+" on ecs:ListTaskDefinitions", detail)
}

// findTagged returns the newest of the family's latest revisions that was
// registered for this run and leg, or "".
func (r *leg) findTagged(family string) (string, error) {
	list, err := r.client.ListTaskDefinitions(r.ctx, &ecs.ListTaskDefinitionsInput{
		FamilyPrefix: aws.String(family), Sort: ecstypes.SortOrderDesc, MaxResults: aws.Int32(lookupRevisions),
	})
	if err != nil {
		return "", err
	}
	run, legID := strconv.FormatInt(r.task.Run.ID, 10), strconv.FormatInt(r.task.Leg.ID, 10)
	for _, arn := range list.TaskDefinitionArns {
		d, err := r.client.DescribeTaskDefinition(r.ctx, &ecs.DescribeTaskDefinitionInput{
			TaskDefinition: aws.String(arn), Include: []ecstypes.TaskDefinitionField{ecstypes.TaskDefinitionFieldTags},
		})
		if err != nil {
			return "", err
		}
		if d.TaskDefinition == nil || aws.ToString(d.TaskDefinition.Family) != family {
			continue
		}
		if tagValue(d.Tags, TagRun) == run && tagValue(d.Tags, TagLeg) == legID {
			return arn, nil
		}
	}
	return "", nil
}

func tagValue(tags []ecstypes.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// familyOf returns the family of a task definition ARN (...:task-definition/<family>:<revision>).
func familyOf(arn string) string {
	i := strings.LastIndex(arn, "task-definition/")
	if i < 0 {
		return ""
	}
	name := arn[i+len("task-definition/"):]
	if j := strings.LastIndexByte(name, ':'); j >= 0 {
		name = name[:j]
	}
	return name
}

// --- changes through Env.Do -----------------------------------------------------

// attempt is one call of Env.Do.
type attempt struct {
	err    error // from Do
	fnRan  bool
	fnErr  error // what fn returned, nil on success
	refuse bool  // fnErr is a definite refusal
	arn    string
}

func (r *leg) registerOnce(in *ecs.RegisterTaskDefinitionInput, key string) attempt {
	var a attempt
	intent := map[string]string{"family": aws.ToString(in.Family), "image": imagesOf(in.ContainerDefinitions)}
	op, err := r.env.Do(r.ctx, registerOp, key, intent, func() (any, error) {
		a.fnRan = true
		out, err := r.client.RegisterTaskDefinition(r.ctx, in)
		if err != nil {
			return nil, r.classify(&a, err)
		}
		if out.TaskDefinition == nil {
			return nil, r.classify(&a, errors.New("no task definition in the answer"))
		}
		a.arn = aws.ToString(out.TaskDefinition.TaskDefinitionArn)
		return map[string]string{"task_definition_arn": a.arn}, nil
	})
	a.err = err
	if !a.fnRan && err == nil {
		// Done by an earlier attempt: the record holds the revision.
		var stored struct {
			ARN string `json:"task_definition_arn"`
		}
		_ = json.Unmarshal(op.Result, &stored)
		a.arn = stored.ARN
	}
	return r.settle(a)
}

func (r *leg) updateOnce(arn, key string) attempt {
	var a attempt
	intent := map[string]string{"cluster": r.ref.Cluster, "service": r.ref.Service, "task_definition": arn}
	_, err := r.env.Do(r.ctx, updateOp, key, intent, func() (any, error) {
		a.fnRan = true
		_, err := r.client.UpdateService(r.ctx, &ecs.UpdateServiceInput{
			Cluster: aws.String(r.ref.Cluster), Service: aws.String(r.ref.Service), TaskDefinition: aws.String(arn),
		})
		if err != nil {
			return nil, r.classify(&a, err)
		}
		return map[string]string{"task_definition": arn}, nil
	})
	a.err = err
	return r.settle(a)
}

// classify marks a call's error as a refusal (HTTP 4xx: ECS looked at the
// request and said no) or wraps it as maybe applied.
func (r *leg) classify(a *attempt, err error) error {
	if isRefusal(err) {
		a.fnErr, a.refuse = err, true
		return err
	}
	a.fnErr = fmt.Errorf("%w: ecs gave no answer: %w", runner.ErrMaybeApplied, err)
	return a.fnErr
}

func (r *leg) settle(a attempt) attempt {
	if a.refuse && r.ctx.Err() != nil {
		a.refuse = false
	}
	return a
}

// refused turns a failed attempt into an outcome; nil means it went through.
// label is "register" or "update"; action the ECS API name.
func (r *leg) refused(a attempt, label, action string) *queue.Outcome {
	switch {
	case a.err == nil:
		return nil
	case errors.Is(a.err, runner.ErrPreviouslyFailed):
		r.env.Log("error", fmt.Sprintf("An earlier attempt's %s was refused and is not repeated.", label))
		return r.stopped("failed", fmt.Sprintf("an earlier attempt's %s was refused", label), "")
	case a.refuse:
		code := codeOf(a.fnErr)
		r.env.Log("error", fmt.Sprintf("ecs:%s was refused (%s).", action, code))
		return r.stopped("failed", code+" on ecs:"+action, resolve.ErrorDetail("ecs:"+action, a.fnErr))
	case !a.fnRan:
		r.env.Log("error", fmt.Sprintf("The %s could not be recorded.", label))
		return r.stopped("unknown", "could not record the "+label, resolve.ErrorDetail("recording the "+label, a.err))
	default:
		r.env.Log("error", fmt.Sprintf("It is not known whether the %s went through.", label))
		return r.stopped("unknown", "could not confirm the "+label, resolve.ErrorDetail("ecs:"+action, a.err))
	}
}

// isRefusal reports whether err is an HTTP 4xx answer.
func isRefusal(err error) bool {
	var response *awshttp.ResponseError
	if !errors.As(err, &response) {
		return false
	}
	status := response.HTTPStatusCode()
	return status >= 400 && status < 500 && status != http.StatusRequestTimeout
}

func imagesOf(containers []ecstypes.ContainerDefinition) string {
	var images []string
	for _, c := range containers {
		images = append(images, aws.ToString(c.Image))
	}
	return strings.Join(images, ",")
}

// --- the service ----------------------------------------------------------------

func (r *leg) updateService() *queue.Outcome {
	arn := r.res.TaskDefinition
	key := updateOp + ":" + arn
	cancelled := func() *queue.Outcome {
		r.env.Log("warn", "Cancelled before the service was updated.")
		return r.stopped("failed", "cancelled before the service was updated",
			"The service was not changed; revision "+arn+" was not applied.")
	}
	if !r.hasKey(key) && r.env.CancelRequested(r.ctx) {
		return cancelled()
	}

	a := r.updateOnce(arn, key)
	if errors.Is(a.err, runner.ErrAmbiguous) {
		svc, err := r.describeService()
		if err != nil {
			r.env.Log("error", fmt.Sprintf("An interrupted update could not be checked (%s).", codeOf(err)))
			return r.stopped("unknown", "could not check an interrupted update", resolve.ErrorDetail("ecs:DescribeServices", err))
		}
		if usesRevision(svc, arn) {
			delete(r.pending, updateOp)
			r.env.Log("ok", "An earlier attempt's update of the service is in place.")
			return nil
		}
		delete(r.pending, updateOp) // the service is not on it: the intended update did not happen
		if r.env.CancelRequested(r.ctx) {
			return cancelled()
		}
		a = r.updateOnce(arn, fmt.Sprintf("%s:retry-%d", key, r.task.Leg.Attempt))
	}
	if out := r.refused(a, "update", "UpdateService"); out != nil {
		return out
	}
	if a.fnRan {
		r.env.Log("info", fmt.Sprintf("Updated %s to %s.", r.ref.Service, arn))
	} else {
		r.env.Log("info", fmt.Sprintf("An earlier attempt updated %s to %s.", r.ref.Service, arn))
	}
	return nil
}

func usesRevision(svc ecstypes.Service, arn string) bool {
	if aws.ToString(svc.TaskDefinition) == arn {
		return true
	}
	for _, d := range svc.Deployments {
		if aws.ToString(d.TaskDefinition) == arn {
			return true
		}
	}
	return false
}

func (r *leg) describeService() (ecstypes.Service, error) {
	out, err := r.client.DescribeServices(r.ctx, &ecs.DescribeServicesInput{
		Cluster: aws.String(r.ref.Cluster), Services: []string{r.ref.Service},
	})
	if err != nil {
		return ecstypes.Service{}, err
	}
	for _, s := range out.Services {
		if aws.ToString(s.ServiceName) == r.ref.Service || aws.ToString(s.ServiceArn) == r.service.ECSService || len(out.Services) == 1 {
			return s, nil
		}
	}
	return ecstypes.Service{}, errors.New("the service was not in the answer")
}

// --- the wait ---------------------------------------------------------------------

func (r *leg) wait() queue.Outcome {
	clock := r.x.d.Clock
	ours := r.res.TaskDefinition
	start := clock.Now()
	var failingSince time.Time
	failing := false
	seen := map[string]bool{}
	// The stable window. A round counts towards it only when Judge says
	// succeeded and both the service and our stopped tasks were read; an unread
	// round or a new stopped task of ours restarts it. Judge's own timeout rule
	// never runs while it says succeeded, so the leg timeout also bounds the
	// window: past it, a rollout that looked complete but was never confirmed
	// ends unknown, with why in the detail.
	var stableSince time.Time
	stableCrashes, stableLogged := 0, false
	lookedComplete := false // the last good read's verdict was succeeded
	var whyUnconfirmed unconfirmed

	for {
		if r.ctx.Err() != nil {
			return r.interrupted()
		}
		r.noteCancel()

		obs := Observation{Elapsed: clock.Now().Sub(start), PreviousTaskDefinition: r.service.PreviousTaskDefinition}
		svc, err := r.describeService()
		switch {
		case err != nil && r.ctx.Err() != nil:
			return r.interrupted()
		case err != nil:
			r.lastReadErr = err
			stableSince = time.Time{} // a round that saw nothing cannot count towards stability
			if !failing {
				failing, failingSince = true, clock.Now()
				r.env.Log("warn", fmt.Sprintf("The service could not be read (%s); trying again.", codeOf(err)))
			}
			obs.ReadFailures = clock.Now().Sub(failingSince)
			if obs.ReadFailures >= ReadFailureLimit {
				v := Judge(obs, ours, r.timeout)
				r.env.Log("error", "The service could not be read for "+ReadFailureLimit.String()+"; the outcome is not known.")
				return r.end(v.State, v.Short, resolve.ErrorDetail("ecs:DescribeServices", err))
			}
			if lookedComplete {
				whyUnconfirmed.service = resolve.ErrorDetail("ecs:DescribeServices", err)
				if obs.Elapsed > r.timeout {
					return r.unconfirmedAtTimeout(whyUnconfirmed)
				}
			}
		default:
			failing = false
			obs.Service = svc
			deployment, _, _ := ourDeployments(svc, ours)
			var since time.Time
			if deployment != nil && deployment.CreatedAt != nil {
				since = *deployment.CreatedAt
			}
			r.logEvents(svc, deployment != nil, since, seen)
			var tasksUnread string
			obs.StoppedTasks, tasksUnread = r.stoppedTasks(ours, since)
			v := Judge(obs, ours, r.timeout)
			windowed := v.State == "succeeded" && svc.DesiredCount > 0
			if windowed && tasksUnread != "" {
				// Nothing was seen of the stopped tasks this round: it cannot count.
				stableSince = time.Time{}
				whyUnconfirmed.tasks = tasksUnread
				v = Verdict{State: "waiting"}
			} else if windowed {
				// A revision can look steady for a moment between crashes: the
				// verdict must hold for StableFor with no new stopped task of ours.
				now, crashes := clock.Now(), len(unreplacedStops(obs.StoppedTasks, ours))
				if stableSince.IsZero() || crashes > stableCrashes {
					if !stableSince.IsZero() {
						whyUnconfirmed.crashes = crashes
					}
					stableSince, stableCrashes = now, crashes
					if !stableLogged {
						stableLogged = true
						r.env.Log("info", "rollout looks complete; confirming it stays stable for "+r.x.d.StableFor.String())
					}
				}
				if now.Sub(stableSince) < r.x.d.StableFor {
					v = Verdict{State: "waiting"}
				}
			} else if v.State == "waiting" {
				stableSince = time.Time{}
			}
			lookedComplete = windowed
			if !windowed {
				whyUnconfirmed = unconfirmed{}
			} else if v.State == "waiting" && obs.Elapsed > r.timeout {
				return r.unconfirmedAtTimeout(whyUnconfirmed)
			}
			if v.State != "waiting" {
				return r.finish(v, svc, obs.StoppedTasks)
			}
		}
		if err := clock.Sleep(r.ctx, r.x.d.PollEvery); err != nil {
			return r.interrupted()
		}
	}
}

func (r *leg) interrupted() queue.Outcome {
	r.env.Log("warn", "Stopped while waiting for the rollout; the outcome is not known.")
	return r.end("unknown", "stopped while waiting for the rollout", "")
}

// noteCancel logs once that a cancel was asked for; it changes nothing.
func (r *leg) noteCancel() {
	if r.cancelLogged || !r.env.CancelRequested(r.ctx) {
		return
	}
	r.cancelLogged = true
	r.env.Log("warn", "a cancel was requested; waiting for the rollout to finish so the outcome is known")
}

// logEvents logs each service event once, oldest first, for events at or after
// the start of our deployment.
func (r *leg) logEvents(svc ecstypes.Service, haveDeployment bool, since time.Time, seen map[string]bool) {
	if !haveDeployment {
		return
	}
	for i := len(svc.Events) - 1; i >= 0; i-- {
		e := svc.Events[i]
		id := aws.ToString(e.Id)
		if seen[id] || e.CreatedAt == nil || e.CreatedAt.Before(since) {
			continue
		}
		seen[id] = true
		r.env.Log("info", "ECS: "+aws.ToString(e.Message))
	}
}

// stoppedTasks returns the tasks of our task definition that stopped at or
// after since, newest first. A failed read only loses this part of the round;
// unread then describes the failed call, and the stable window must not count
// the round.
func (r *leg) stoppedTasks(ours string, since time.Time) (tasks []ecstypes.Task, unread string) {
	var arns []string
	var token *string
	for page := 0; page < maxTaskPages; page++ {
		list, err := r.client.ListTasks(r.ctx, &ecs.ListTasksInput{
			Cluster: aws.String(r.ref.Cluster), ServiceName: aws.String(r.ref.Service),
			DesiredStatus: ecstypes.DesiredStatusStopped, NextToken: token,
		})
		if err != nil {
			return nil, resolve.ErrorDetail("ecs:ListTasks", err)
		}
		arns = append(arns, list.TaskArns...)
		if token = list.NextToken; token == nil {
			break
		}
	}
	for chunk := range slices.Chunk(arns, 100) {
		out, err := r.client.DescribeTasks(r.ctx, &ecs.DescribeTasksInput{Cluster: aws.String(r.ref.Cluster), Tasks: chunk})
		if err != nil {
			return nil, resolve.ErrorDetail("ecs:DescribeTasks", err)
		}
		for _, t := range out.Tasks {
			if aws.ToString(t.TaskDefinitionArn) != ours {
				continue
			}
			if t.StoppedAt != nil && t.StoppedAt.Before(since) {
				continue
			}
			tasks = append(tasks, t)
		}
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		a, b := tasks[i].StoppedAt, tasks[j].StoppedAt
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return a.After(*b)
	})
	return tasks, ""
}

// unconfirmed records what kept the stable window from completing since the
// rollout last looked complete.
type unconfirmed struct {
	service string // the last failed service read
	tasks   string // the last failed stopped-task read
	crashes int    // our stopped tasks when one last restarted the window
}

// unconfirmedAtTimeout ends a leg whose rollout looked complete when the
// timeout passed but was never confirmed stable. Three of our stopped tasks
// would already have failed it in Judge, so this is unknown, not failed.
func (r *leg) unconfirmedAtTimeout(why unconfirmed) queue.Outcome {
	var parts []string
	if why.tasks != "" {
		parts = append(parts, "the stopped tasks could not be read ("+why.tasks+")")
	}
	if why.service != "" {
		parts = append(parts, "the service could not be read ("+why.service+")")
	}
	if why.crashes > 0 {
		parts = append(parts, fmt.Sprintf("our tasks kept stopping (%d stopped)", why.crashes))
	}
	if len(parts) == 0 {
		parts = append(parts, "the stable window did not complete")
	}
	short := "rollout looked complete but could not be confirmed stable within " + r.timeout.String()
	r.env.Log("error", short)
	return r.end("unknown", short, strings.Join(parts, "; "))
}

// finish logs and returns the outcome for a verdict that ends the wait.
func (r *leg) finish(v Verdict, svc ecstypes.Service, stopped []ecstypes.Task) queue.Outcome {
	switch v.State {
	case "succeeded":
		r.env.Log("ok", fmt.Sprintf("%s runs %s with %d of %d tasks", r.ref.Service, r.res.TaskDefinition, svc.RunningCount, svc.DesiredCount))
		return r.end("succeeded", "", "")
	case "failed":
		r.env.Log("error", v.Short+": "+v.Detail)
		for i, t := range stopped {
			if i == stoppedTasksShown {
				break
			}
			line := fmt.Sprintf("%s: %s", taskID(t), aws.ToString(t.StoppedReason))
			if c := exitedContainer(t); c != nil {
				line += fmt.Sprintf("; container %s exit %d", aws.ToString(c.Name), *c.ExitCode)
			}
			r.env.Log("error", line)
		}
		return r.end("failed", v.Short, v.Detail)
	default:
		r.env.Log("error", v.Short)
		return r.end("unknown", v.Short, v.Detail)
	}
}

func taskID(t ecstypes.Task) string {
	arn := aws.ToString(t.TaskArn)
	return arn[strings.LastIndexByte(arn, '/')+1:]
}

// --- helpers ----------------------------------------------------------------------

func (r *leg) hasKey(key string) bool {
	for _, op := range r.ops {
		if op.Key == key {
			return true
		}
	}
	return false
}

func (r *leg) hasOp(name string) bool {
	for _, op := range r.ops {
		if op.Name == name {
			return true
		}
	}
	return false
}

func codeOf(err error) string {
	if code := resolve.ErrorCode(err); code != "" {
		return code
	}
	return "RequestFailed"
}

func (r *leg) stopped(state, short, detail string) *queue.Outcome {
	out := r.end(state, short, detail)
	return &out
}

// end builds the outcome; the result is included whenever the revision is known.
func (r *leg) end(state, short, detail string) queue.Outcome {
	if state == "failed" && len(r.pending) > 0 {
		// An earlier attempt's change is still unresolved: failing would
		// release the run's locks over a change that may be in place.
		state = "unknown"
	}
	out := queue.Outcome{State: state, FailureShort: short, FailureDetail: detail}
	if r.res.TaskDefinition != "" {
		out.Result, _ = json.Marshal(r.res)
	}
	return out
}
