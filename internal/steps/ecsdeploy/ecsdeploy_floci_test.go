//go:build integration && floci

package ecsdeploy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner/runnertest"
	"github.com/rcss-labs/yamdrake/internal/steps/ecsdeploy"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/test/awstest"
)

const flociRegion = "us-east-1"

// flociService is a running one-task service in a random account, with a
// second image (tag "next") ready to deploy.
type flociService struct {
	w           *awstest.World
	acct        *awstest.Account
	repo        string
	repoURI     string
	serviceARN  string
	prevTaskDef string
	cluster     string
}

func newFlociService(t *testing.T) *flociService {
	t.Helper()
	w := awstest.New(t)
	acct := w.Account(awstest.RandomAccountID())
	sfx := awstest.RandomSuffix()
	s := &flociService{w: w, acct: acct, repo: "yd-web-" + sfx, cluster: "yd-c-" + sfx}
	s.repoURI, _ = acct.Repo(flociRegion, s.repo, "v1")
	s.serviceARN, s.prevTaskDef = acct.Service(flociRegion, s.cluster, "web", s.repoURI+":v1")
	// The previous revision carries a tag of its own, which must survive.
	if _, err := ecs.NewFromConfig(acct.AWSConfig(flociRegion)).TagResource(context.Background(), &ecs.TagResourceInput{
		ResourceArn: aws.String(s.prevTaskDef), Tags: []ecstypes.Tag{{Key: aws.String("team"), Value: aws.String("platform")}},
	}); err != nil {
		t.Fatalf("tag the previous revision: %v", err)
	}
	return s
}

// deploy runs the executor against the service with real time and returns the
// outcome and the leg's log. cfg is the SDK configuration the executor gets
// (through the proxy in the failure tests).
func (s *flociService) deploy(t *testing.T, cfg aws.Config, digest string) (queue.Outcome, *runnertest.Env) {
	t.Helper()
	raw, err := json.Marshal(manifest.Manifest{ID: "floci-manifest", RunID: 9101, AWSAccount: s.acct.ID, Services: []manifest.Service{{
		Name: "web", Version: "2.0.0", ImageDigest: digest, ECSService: s.serviceARN, ECRRepository: s.repoURI,
		PreviousTaskDefinition: s.prevTaskDef,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	task := queue.Claimed{
		Leg:  store.Leg{ID: 9102, RunID: 9101, Kind: "step", Service: "web", Attempt: 1},
		Run:  store.Run{ID: 9101, AWSAccount: s.acct.ID, Manifest: raw},
		Step: &store.RunStep{Timeout: 3 * time.Minute},
	}
	env := &runnertest.Env{Attempt: 1}
	out := ecsdeploy.New(ecsdeploy.Deps{
		AWS:       func(context.Context, string, string, string) (aws.Config, error) { return cfg, nil },
		Options:   awsx.Options{EndpointURL: s.w.Endpoint},
		PollEvery: 2 * time.Second,
	}).Execute(context.Background(), env, task)
	return out, env
}

func logOf(env *runnertest.Env) string {
	var b strings.Builder
	for _, l := range env.Lines() {
		b.WriteString(l.Level + ": " + l.Text + "\n")
	}
	return b.String()
}

func resultOf(t *testing.T, out queue.Outcome) ecsdeploy.Result {
	t.Helper()
	var res ecsdeploy.Result
	if err := json.Unmarshal(out.Result, &res); err != nil {
		t.Fatalf("result %q: %v", out.Result, err)
	}
	return res
}

func TestDeployAgainstFloci(t *testing.T) {
	s := newFlociService(t)
	digest := s.acct.RepoImage(flociRegion, s.repo, "next", "second")

	out, env := s.deploy(t, s.acct.AWSConfig(flociRegion), digest)
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q: %s\n%s", out.State, out.FailureShort, out.FailureDetail, logOf(env))
	}
	res := resultOf(t, out)
	if res.TaskDefinition == "" || res.TaskDefinition == s.prevTaskDef || res.PreviousTaskDefinition != s.prevTaskDef || res.Adopted {
		t.Fatalf("result = %+v", res)
	}

	client := ecs.NewFromConfig(s.acct.AWSConfig(flociRegion))
	svc, err := client.DescribeServices(context.Background(), &ecs.DescribeServicesInput{Cluster: aws.String(s.cluster), Services: []string{"web"}})
	if err != nil || len(svc.Services) != 1 {
		t.Fatalf("DescribeServices: %v", err)
	}
	if got := aws.ToString(svc.Services[0].TaskDefinition); got != res.TaskDefinition {
		t.Errorf("service runs %s, want %s", got, res.TaskDefinition)
	}
	td, err := client.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(res.TaskDefinition), Include: []ecstypes.TaskDefinitionField{ecstypes.TaskDefinitionFieldTags},
	})
	if err != nil {
		t.Fatalf("DescribeTaskDefinition: %v", err)
	}
	if got := aws.ToString(td.TaskDefinition.ContainerDefinitions[0].Image); got != s.repoURI+"@"+digest {
		t.Errorf("image = %q, want %s@%s", got, s.repoURI, digest)
	}
	for key, want := range map[string]string{
		ecsdeploy.TagVersion: "2.0.0", ecsdeploy.TagRun: "9101", ecsdeploy.TagLeg: "9102", ecsdeploy.TagManifest: "floci-manifest", "team": "platform",
	} {
		if got := tagValue(td.Tags, key); got != want {
			t.Errorf("tag %s = %q, want %q", key, got, want)
		}
	}
}

// TestRolloutFailureThroughProxyRewrite: Floci never reports FAILED, so the
// proxy rewrites DescribeServices to say our deployment failed.
func TestRolloutFailureThroughProxyRewrite(t *testing.T) {
	s := newFlociService(t)
	digest := s.acct.RepoImage(flociRegion, s.repo, "next", "second")
	proxy := s.w.Proxy(t)
	proxy.Rewrite("ecs", "DescribeServices", func(body []byte) []byte {
		var doc map[string]any
		if json.Unmarshal(body, &doc) != nil {
			return body
		}
		services, _ := doc["services"].([]any)
		for _, svc := range services {
			deployments, _ := svc.(map[string]any)["deployments"].([]any)
			for _, d := range deployments {
				dep := d.(map[string]any)
				if dep["taskDefinition"] != s.prevTaskDef { // ours
					dep["rolloutState"] = "FAILED"
					dep["rolloutStateReason"] = "ECS deployment circuit breaker: tasks failed to start (injected by the test)"
				}
			}
		}
		out, err := json.Marshal(doc)
		if err != nil {
			return body
		}
		return out
	})

	out, env := s.deploy(t, s.acct.AWSConfigAt(flociRegion, proxy.URL), digest)
	if out.State != "failed" {
		t.Fatalf("outcome = %q %q: %s\n%s", out.State, out.FailureShort, out.FailureDetail, logOf(env))
	}
	if !strings.Contains(out.FailureDetail, "circuit breaker: tasks failed to start") {
		t.Errorf("detail = %q", out.FailureDetail)
	}
	if resultOf(t, out).TaskDefinition == "" {
		t.Error("the failed leg should still say which revision it applied")
	}
}

func TestCrashingImageFailsByStoppedTasks(t *testing.T) {
	s := newFlociService(t)
	digest := s.acct.RepoCrashingImage(flociRegion, s.repo, "crash", "crash", 3)

	out, env := s.deploy(t, s.acct.AWSConfig(flociRegion), digest)
	if out.State != "failed" || out.FailureShort != "tasks keep stopping" {
		t.Fatalf("outcome = %q %q: %s\n%s", out.State, out.FailureShort, out.FailureDetail, logOf(env))
	}
	if !strings.Contains(out.FailureDetail, fmt.Sprintf("exited with %d", 3)) {
		t.Errorf("detail = %q", out.FailureDetail)
	}
}
