package ecsdeploy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/rcss-labs/yamdrake/internal/steps/ecsdeploy"
)

const (
	ours     = "arn:aws:ecs:eu-west-1:123456789012:task-definition/web:8"
	previous = "arn:aws:ecs:eu-west-1:123456789012:task-definition/web:7"
	timeout  = 15 * time.Minute
)

func deployment(taskDef, status string, state ecstypes.DeploymentRolloutState, reason string) ecstypes.Deployment {
	return ecstypes.Deployment{
		TaskDefinition: aws.String(taskDef), Status: aws.String(status),
		RolloutState: state, RolloutStateReason: aws.String(reason),
		DesiredCount: 2, RunningCount: 2,
	}
}

// healthy is a finished rollout of our revision.
func healthy() ecsdeploy.Observation {
	return ecsdeploy.Observation{
		Service: ecstypes.Service{
			TaskDefinition: aws.String(ours), DesiredCount: 2, RunningCount: 2,
			Deployments: []ecstypes.Deployment{deployment(ours, "PRIMARY", ecstypes.DeploymentRolloutStateCompleted, "")},
		},
		PreviousTaskDefinition: previous,
	}
}

func crashed(reason string, code int32) ecstypes.Task {
	return ecstypes.Task{
		TaskArn: aws.String("arn:aws:ecs:eu-west-1:123456789012:task/c/abc"), TaskDefinitionArn: aws.String(ours),
		StoppedReason: aws.String(reason),
		Containers:    []ecstypes.Container{{Name: aws.String("app"), ExitCode: aws.Int32(code)}},
	}
}

func TestJudgeRule1ReadFailuresForTwoMinutesGiveUnknown(t *testing.T) {
	o := healthy() // would succeed, but reads have been failing
	o.ReadFailures = ecsdeploy.ReadFailureLimit
	if v := ecsdeploy.Judge(o, ours, timeout); v.State != "unknown" {
		t.Errorf("state = %q, want unknown", v.State)
	}
	o.ReadFailures = ecsdeploy.ReadFailureLimit - time.Second
	if v := ecsdeploy.Judge(o, ours, timeout); v.State != "succeeded" {
		t.Errorf("state = %q just under the limit, want succeeded", v.State)
	}
}

func TestJudgeRule2DesiredZeroOnOurRevisionSucceeds(t *testing.T) {
	o := ecsdeploy.Observation{Service: ecstypes.Service{
		TaskDefinition: aws.String(ours), DesiredCount: 0,
		Deployments: []ecstypes.Deployment{deployment(ours, "PRIMARY", ecstypes.DeploymentRolloutStateInProgress, "")},
	}}
	v := ecsdeploy.Judge(o, ours, timeout)
	if v.State != "succeeded" || v.Detail != "desired count is 0, nothing to wait for" {
		t.Errorf("verdict = %+v", v)
	}
	o.Service.TaskDefinition = aws.String(previous)
	if v := ecsdeploy.Judge(o, ours, timeout); v.State == "succeeded" {
		t.Errorf("desired 0 on another revision must not succeed: %+v", v)
	}
}

func TestJudgeRule3PrimaryIsNoLongerOursMeansRolledBack(t *testing.T) {
	o := healthy()
	o.Service.TaskDefinition = aws.String(previous)
	o.Service.Deployments = []ecstypes.Deployment{
		deployment(previous, "PRIMARY", ecstypes.DeploymentRolloutStateCompleted, ""),
		deployment(ours, "ACTIVE", ecstypes.DeploymentRolloutStateInProgress, ""),
	}
	v := ecsdeploy.Judge(o, ours, timeout)
	if v.State != "failed" || v.Short != "ECS rolled back" {
		t.Fatalf("verdict = %+v", v)
	}
	if !strings.Contains(v.Detail, previous) || !strings.Contains(v.Detail, "back on the previous revision") {
		t.Errorf("detail = %q", v.Detail)
	}
	// A different primary is named without the "previous revision" claim.
	o.Service.Deployments[0] = deployment("arn:aws:ecs:eu-west-1:123456789012:task-definition/web:9", "PRIMARY", ecstypes.DeploymentRolloutStateInProgress, "")
	v = ecsdeploy.Judge(o, ours, timeout)
	if v.State != "failed" || strings.Contains(v.Detail, "previous revision") || !strings.Contains(v.Detail, "web:9") {
		t.Errorf("verdict = %+v", v)
	}
}

func TestJudgeRule4FailedRolloutStateGivesECSReason(t *testing.T) {
	o := healthy()
	o.Service.Deployments = []ecstypes.Deployment{deployment(ours, "PRIMARY", ecstypes.DeploymentRolloutStateFailed, "tasks failed to start")}
	v := ecsdeploy.Judge(o, ours, timeout)
	if v.State != "failed" || v.Detail != "tasks failed to start" {
		t.Errorf("verdict = %+v", v)
	}
}

func TestJudgeRule5ThreeStoppedTasksFail(t *testing.T) {
	o := healthy()
	o.Service.Deployments = []ecstypes.Deployment{deployment(ours, "PRIMARY", ecstypes.DeploymentRolloutStateInProgress, "")}
	o.StoppedTasks = []ecstypes.Task{
		crashed("Essential container in task exited", 3), crashed("Essential container in task exited", 3), crashed("Essential container in task exited", 3),
	}
	v := ecsdeploy.Judge(o, ours, timeout)
	if v.State != "failed" || v.Short != "tasks keep stopping" {
		t.Fatalf("verdict = %+v", v)
	}
	if v.Detail != "Essential container in task exited; container app exited with 3" {
		t.Errorf("detail = %q", v.Detail)
	}
	o.StoppedTasks = o.StoppedTasks[:2]
	if v := ecsdeploy.Judge(o, ours, timeout); v.State != "waiting" {
		t.Errorf("two stopped tasks: state = %q, want waiting", v.State)
	}
}

func TestJudgeIgnoresTasksStoppedByDeployment(t *testing.T) {
	o := healthy()
	o.Service.Deployments = []ecstypes.Deployment{deployment(ours, "PRIMARY", ecstypes.DeploymentRolloutStateInProgress, "")}
	o.StoppedTasks = []ecstypes.Task{
		crashed("Scaling activity initiated by (deployment ecs-svc/123)", 0),
		crashed("Task stopped by DEPLOYMENT replacement", 0),
		crashed("Service deployment rollback", 0),
		crashed("Essential container in task exited", 3),
	}
	if v := ecsdeploy.Judge(o, ours, timeout); v.State != "waiting" {
		t.Errorf("verdict = %+v, want waiting", v)
	}
	// Tasks of another revision do not count either.
	other := crashed("Essential container in task exited", 1)
	other.TaskDefinitionArn = aws.String(previous)
	o.StoppedTasks = []ecstypes.Task{other, other, other}
	if v := ecsdeploy.Judge(o, ours, timeout); v.State != "waiting" {
		t.Errorf("verdict = %+v, want waiting", v)
	}
}

func TestJudgeRule6CompletedOnOurRevisionSucceeds(t *testing.T) {
	if v := ecsdeploy.Judge(healthy(), ours, timeout); v.State != "succeeded" {
		t.Errorf("verdict = %+v", v)
	}
	o := healthy()
	o.Service.RunningCount = 1
	if v := ecsdeploy.Judge(o, ours, timeout); v.State != "waiting" {
		t.Errorf("running 1 of 2: state = %q, want waiting", v.State)
	}
	o = healthy()
	o.Service.Deployments[0].RolloutState = ecstypes.DeploymentRolloutStateInProgress
	if v := ecsdeploy.Judge(o, ours, timeout); v.State != "waiting" {
		t.Errorf("in progress: state = %q, want waiting", v.State)
	}
}

func TestJudgeDoesNotSucceedWhileAnOlderDeploymentRemains(t *testing.T) {
	o := healthy()
	o.Service.Deployments = append(o.Service.Deployments, deployment(previous, "ACTIVE", ecstypes.DeploymentRolloutStateCompleted, ""))
	if v := ecsdeploy.Judge(o, ours, timeout); v.State != "waiting" {
		t.Errorf("verdict = %+v, want waiting", v)
	}
}

func TestJudgeDoesNotSucceedOnBriefStabilityOfAFailingRevision(t *testing.T) {
	o := healthy() // COMPLETED right now, but the revision has been crashing
	o.StoppedTasks = []ecstypes.Task{
		crashed("Essential container in task exited", 3), crashed("Essential container in task exited", 3), crashed("Essential container in task exited", 3),
	}
	v := ecsdeploy.Judge(o, ours, timeout)
	if v.State != "failed" || v.Short != "tasks keep stopping" {
		t.Errorf("verdict = %+v", v)
	}
}

func TestJudgeRule7TimeoutFails(t *testing.T) {
	o := healthy()
	o.Service.RunningCount = 1
	o.Elapsed = timeout
	if v := ecsdeploy.Judge(o, ours, timeout); v.State != "waiting" {
		t.Errorf("exactly at the timeout: %+v, want waiting", v)
	}
	o.Elapsed = timeout + time.Second
	v := ecsdeploy.Judge(o, ours, timeout)
	if v.State != "failed" || v.Short != "timed out after 15m0s" || v.Detail != "running 1 of 2 desired" {
		t.Errorf("verdict = %+v", v)
	}
	o.StoppedTasks = []ecstypes.Task{crashed("CannotPullContainerError", 1)}
	v = ecsdeploy.Judge(o, ours, timeout)
	if v.Detail != "running 1 of 2 desired; last stopped task: CannotPullContainerError" {
		t.Errorf("detail = %q", v.Detail)
	}
}

func TestJudgeRule8OtherwiseWaiting(t *testing.T) {
	o := healthy()
	o.Service.RunningCount = 0
	o.Service.Deployments[0].RolloutState = ecstypes.DeploymentRolloutStateInProgress
	o.Elapsed = time.Minute
	if v := ecsdeploy.Judge(o, ours, timeout); v.State != "waiting" {
		t.Errorf("verdict = %+v", v)
	}
}
