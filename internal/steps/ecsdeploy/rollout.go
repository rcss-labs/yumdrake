package ecsdeploy

import (
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// ReadFailureLimit is how long reads of the service may fail in a row before
// the outcome is declared unknown.
const ReadFailureLimit = 2 * time.Minute

// Observation is what one poll saw.
type Observation struct {
	Service ecstypes.Service
	// StoppedTasks are the tasks of our task definition that stopped since the
	// update, newest first.
	StoppedTasks []ecstypes.Task
	Elapsed      time.Duration
	// ReadFailures is how long reads have been failing in a row.
	ReadFailures time.Duration
	// PreviousTaskDefinition is the revision the service ran before; it only
	// sharpens the wording of a rollback.
	PreviousTaskDefinition string
}

// Verdict is Judge's reading of an observation. State is "waiting",
// "succeeded", "failed" or "unknown".
type Verdict struct{ State, Short, Detail string }

// stoppedTaskLimit is how many stopped tasks of our revision (not replaced by
// a deployment) make the rollout a failure.
const stoppedTaskLimit = 3

// Judge decides what an observation of a rollout means. ourTaskDef is the
// task definition ARN this leg applied. The rules run in a fixed order; see
// the numbered comments.
func Judge(o Observation, ourTaskDef string, timeout time.Duration) Verdict {
	svc := o.Service
	ours, primary, others := ourDeployments(svc, ourTaskDef)

	// 1. We cannot see the service.
	if o.ReadFailures >= ReadFailureLimit {
		return Verdict{"unknown", "could not read the service for 2 minutes", ""}
	}
	// 2. Nothing to roll out.
	if svc.DesiredCount == 0 && aws.ToString(svc.TaskDefinition) == ourTaskDef {
		return Verdict{"succeeded", "", "desired count is 0, nothing to wait for"}
	}
	// 3. ECS put another revision in charge (circuit breaker rollback).
	if primary != nil && aws.ToString(primary.TaskDefinition) != ourTaskDef {
		now := aws.ToString(primary.TaskDefinition)
		detail := "ECS rolled back: the primary deployment is now " + now
		if o.PreviousTaskDefinition != "" && now == o.PreviousTaskDefinition {
			detail += " (the service is back on the previous revision)"
		}
		return Verdict{"failed", "ECS rolled back", detail}
	}
	// 4. ECS says our rollout failed.
	if ours != nil && ours.RolloutState == ecstypes.DeploymentRolloutStateFailed {
		return Verdict{"failed", "the rollout failed", aws.ToString(ours.RolloutStateReason)}
	}
	// 5. Our revision's tasks keep dying. Checked before success so a revision
	// that is briefly stable between crashes is not called good.
	if crashing := unreplacedStops(o.StoppedTasks, ourTaskDef); len(crashing) >= stoppedTaskLimit {
		return Verdict{"failed", "tasks keep stopping", describeStop(crashing[0])}
	}
	// 6. Rolled out and steady.
	if ours != nil && ours.RolloutState == ecstypes.DeploymentRolloutStateCompleted &&
		aws.ToString(svc.TaskDefinition) == ourTaskDef && svc.RunningCount == svc.DesiredCount && others == 0 {
		return Verdict{State: "succeeded"}
	}
	// 7. Out of time.
	if o.Elapsed > timeout {
		detail := fmt.Sprintf("running %d of %d desired", svc.RunningCount, svc.DesiredCount)
		if len(o.StoppedTasks) > 0 {
			detail += "; last stopped task: " + aws.ToString(o.StoppedTasks[0].StoppedReason)
		}
		return Verdict{"failed", "timed out after " + timeout.String(), detail}
	}
	return Verdict{State: "waiting"}
}

// ourDeployments finds our deployment and the primary one, and counts the
// deployments that are not ours.
func ourDeployments(svc ecstypes.Service, ourTaskDef string) (ours, primary *ecstypes.Deployment, others int) {
	for i := range svc.Deployments {
		d := &svc.Deployments[i]
		if aws.ToString(d.Status) == "PRIMARY" {
			primary = d
		}
		if aws.ToString(d.TaskDefinition) == ourTaskDef && (ours == nil || aws.ToString(d.Status) == "PRIMARY") {
			ours = d
		} else if aws.ToString(d.TaskDefinition) != ourTaskDef {
			others++
		}
	}
	return ours, primary, others
}

// unreplacedStops keeps the tasks of our task definition that stopped for a
// reason other than a deployment replacing them.
func unreplacedStops(tasks []ecstypes.Task, ourTaskDef string) []ecstypes.Task {
	var out []ecstypes.Task
	for _, t := range tasks {
		if aws.ToString(t.TaskDefinitionArn) != ourTaskDef {
			continue
		}
		if strings.Contains(strings.ToLower(aws.ToString(t.StoppedReason)), "deployment") {
			continue
		}
		out = append(out, t)
	}
	return out
}

// exitedContainer returns the container whose exit explains the stop: the
// first with a non-zero exit code, else the first with any exit code.
func exitedContainer(t ecstypes.Task) *ecstypes.Container {
	var first *ecstypes.Container
	for i := range t.Containers {
		c := &t.Containers[i]
		if c.ExitCode == nil {
			continue
		}
		if *c.ExitCode != 0 {
			return c
		}
		if first == nil {
			first = c
		}
	}
	return first
}

// describeStop is "<stopped reason>; container <name> exited with <code>".
func describeStop(t ecstypes.Task) string {
	text := aws.ToString(t.StoppedReason)
	if c := exitedContainer(t); c != nil {
		text += fmt.Sprintf("; container %s exited with %d", aws.ToString(c.Name), *c.ExitCode)
	}
	return text
}
