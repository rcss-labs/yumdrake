package resolve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const (
	sourceRunID     = 40
	webOldTaskDef   = "arn:aws:ecs:eu-west-1:111122223333:task-definition/web:4"
	webOldDigest    = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	sourceEnvKey    = "manifests/40/web/env/previous/web.env"
	sourceTomlKey   = "manifests/40/web/toml/previous/web.toml"
	restoredEnvKey  = "manifests/42/web/env/web.env"
	currentEnvKey   = "manifests/42/web/env/previous/web.env"
	didNotExistNote = "did not exist before run #40; left in place"
)

// restoreHarness is the harness plus a finished source run #40 that changed
// web: before it web ran web:4 at 1.2.0 with prevEnvContent in its env file
// and no TOML file; now web runs web:5 and the destinations hold what the run
// wrote.
func restoreHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness()
	h.aws.taskDefs[webOldTaskDef] = ecs.DescribeTaskDefinitionOutput{TaskDefinition: &ecstypes.TaskDefinition{
		TaskDefinitionArn: aws.String(webOldTaskDef), Family: aws.String("web"), Status: ecstypes.TaskDefinitionStatusActive,
		ContainerDefinitions: []ecstypes.ContainerDefinition{{Name: aws.String("app"), Image: aws.String(webRepoURI + "@" + webOldDigest)}},
	}}
	h.aws.objects["config-dest/env/web.env"] = stubObject{data: envContent}
	h.aws.objects["config-dest/toml/web.toml"] = stubObject{data: tomlContent}
	h.artifacts.items[sourceEnvKey] = prevEnvContent

	source := manifest.Manifest{
		RunID: sourceRunID, Environment: "prod-1", Label: "prod", AWSAccount: account,
		InventoryCommit: "source-inv-commit", JobCommit: "source-job-commit",
		Services: []manifest.Service{{
			Name: "web", Group: "all", Version: "1.4.2", ImageDigest: webDigest, ECSService: webARN, ECRRepository: webRepoURI,
			PreviousTaskDefinition: webOldTaskDef, PreviousVersion: "1.2.0",
			Config: []manifest.ConfigItem{
				{
					Kind: "env", Name: "web.env", Source: envSource, Destination: destEnvURI, Branch: "v1.4.2", Commit: branchHead,
					SHA256: sha(envContent), PreviousExists: true, PreviousSHA256: sha(prevEnvContent), PreviousArtifactKey: sourceEnvKey, Change: "changed",
				},
				{
					Kind: "toml", Name: "web.toml", Source: "s3://" + tomlBucket + "/" + tomlKey, Destination: destTomlURI,
					SHA256: sha(tomlContent), Change: "new", // no previous copy: the file did not exist before the run
				},
			},
		}},
	}
	source.ID = manifest.Identity(source)
	h.deps.SourceRun = func(_ context.Context, id int64) (store.Run, error) {
		if id != sourceRunID {
			t.Errorf("SourceRun asked for run %d, want %d", id, sourceRunID)
		}
		return store.Run{ID: sourceRunID, Kind: "deploy", Manifest: mustJSON(t, source), ManifestID: source.ID}, nil
	}
	return h
}

func restoreTask(t *testing.T, services ...string) queue.Claimed {
	t.Helper()
	versions := map[string]string{}
	for _, name := range services {
		versions[name] = "1.2.0"
	}
	task := claimed(t, "prod", engine.Selection{"restore": {Services: versions}})
	parent := int64(sourceRunID)
	task.Run.Kind, task.Run.ParentRunID = "restore", &parent
	return task
}

func TestRestoreManifestTargetsPreviousTaskDefinitionAndPreviousConfig(t *testing.T) {
	h := restoreHarness(t)
	out, m := h.run(t, restoreTask(t, "web"))
	if out.State != "succeeded" {
		t.Fatalf("State = %q (%s: %s), want succeeded", out.State, out.FailureShort, out.FailureDetail)
	}
	if m.ID != manifest.Identity(m) || m.RunID != runID || m.RestoreOf != sourceRunID || m.Environment != "prod-1" || m.Label != "prod" ||
		m.AWSAccount != account || m.InventoryCommit != "source-inv-commit" || m.JobCommit != "source-job-commit" {
		t.Errorf("manifest header = %+v", m)
	}
	if len(m.Services) != 1 {
		t.Fatalf("services = %+v, want only web", m.Services)
	}
	web := m.Services[0]
	if web.Name != "web" || web.Group != "all" || web.ECSService != webARN || web.ECRRepository != webRepoURI ||
		web.Version != "1.2.0" || web.TargetTaskDefinition != webOldTaskDef || web.ImageDigest != webOldDigest {
		t.Errorf("web target = %+v, want the old version, task definition and the digest of its image", web)
	}
	// What the restore replaces becomes its own previous state, so it can be restored in turn.
	if web.PreviousTaskDefinition != webTaskDef || web.PreviousVersion != "1.3.0" {
		t.Errorf("web previous = %q %q, want the task definition it runs now and its version", web.PreviousTaskDefinition, web.PreviousVersion)
	}

	if len(web.Config) != 2 {
		t.Fatalf("config = %+v", web.Config)
	}
	env := web.Config[0]
	if env.Kind != "env" || env.Name != "web.env" || env.Source != envSource || env.Destination != destEnvURI || env.Branch != "" || env.Commit != "" ||
		env.NotRestored != "" || env.SHA256 != sha(prevEnvContent) || env.Size != int64(len(prevEnvContent)) || env.ArtifactKey != restoredEnvKey ||
		!env.PreviousExists || env.PreviousSHA256 != sha(envContent) || env.PreviousArtifactKey != currentEnvKey || env.Change != "changed" {
		t.Errorf("env item = %+v", env)
	}
	if env.Keys == nil || strings.Join(env.Keys.Added, ",") != "ONLY_OLD" || strings.Join(env.Keys.Removed, ",") != "ONLY_NEW" || strings.Join(env.Keys.Changed, ",") != "API_KEY" {
		t.Errorf("env keys = %+v, want the diff from what the destination holds now to the old file", env.Keys)
	}
	// The old file is copied under the restore run; what is there now is kept as the previous copy.
	if got := h.artifacts.items[restoredEnvKey]; !bytes.Equal(got, prevEnvContent) {
		t.Errorf("copy of the file to restore holds %q, want the source run's previous copy", got)
	}
	if got := h.artifacts.items[currentEnvKey]; !bytes.Equal(got, envContent) {
		t.Errorf("previous copy under the restore run holds %q, want what the destination holds now", got)
	}

	for _, name := range []string{"service exists", "task definition exists", "config copy web.env", "destination readable web.env", "destination readable web.toml"} {
		c, ok := check(m, "web", name)
		if name == "config copy web.env" {
			// A found copy needs no check of its own.
			if ok {
				t.Errorf("check %q = %+v, want none when the copy was found", name, c)
			}
			continue
		}
		if !ok || c.Status != "passed" {
			t.Errorf("check web/%s = %+v, %v", name, c, ok)
		}
	}
	if _, ok := check(m, "web", "image exists"); ok || h.aws.callCount("ecr:DescribeImages") != 0 {
		t.Error("a restore must not look an image up by tag; the task definition already names it")
	}
	if len(h.gh.heads) != 0 {
		t.Errorf("a restore read GitHub branches %v", h.gh.heads)
	}
	if len(h.snapshots) != 0 {
		t.Errorf("a restore read inventory snapshots %v; it copies its services from the source plan", h.snapshots)
	}
	log := h.env.text()
	for _, secret := range []string{secretEnvValue, secretPrevEnvValue, secretTomlValue, secretPrevToml} {
		if strings.Contains(log, secret) {
			t.Errorf("the log contains a file value %q", secret)
		}
	}
}

func TestRestoreLeavesFilesThatDidNotExistBefore(t *testing.T) {
	h := restoreHarness(t)
	out, m := h.run(t, restoreTask(t, "web"))
	if out.State != "succeeded" {
		t.Fatalf("State = %q (%s: %s), want succeeded", out.State, out.FailureShort, out.FailureDetail)
	}
	toml := serviceOf(t, m, "web").Config[1]
	if toml.NotRestored != didNotExistNote {
		t.Errorf("not restored = %q, want %q", toml.NotRestored, didNotExistNote)
	}
	if toml.SHA256 != "" || toml.ArtifactKey != "" || toml.Change != "unchanged" || toml.Name != "web.toml" || toml.Destination != destTomlURI {
		t.Errorf("toml item = %+v, want it named, without content, and unchanged", toml)
	}
	// Its destination is still read, so the plan records what is there.
	if !toml.PreviousExists || toml.PreviousSHA256 != sha(tomlContent) || h.artifacts.items["manifests/42/web/toml/previous/web.toml"] == nil {
		t.Errorf("toml item = %+v, want the current destination recorded as previous", toml)
	}
	if len(m.Failed()) != 0 {
		t.Errorf("failed checks: %+v", m.Failed())
	}
}

func TestRestoreOfNotRestoredItemKeepsTheReasonOfTheRestoreBeingRestored(t *testing.T) {
	h := restoreHarness(t)
	h.deps.SourceRun = func(_ context.Context, id int64) (store.Run, error) {
		source := manifest.Manifest{
			RunID: sourceRunID, Environment: "prod-1", Label: "prod", AWSAccount: account, RestoreOf: 30,
			Services: []manifest.Service{{
				Name: "web", ECSService: webARN, ECRRepository: webRepoURI, PreviousTaskDefinition: webOldTaskDef, PreviousVersion: "1.2.0",
				Config: []manifest.ConfigItem{{Kind: "toml", Name: "web.toml", Destination: destTomlURI, NotRestored: "left in place"}},
			}},
		}
		return store.Run{ID: id, Kind: "restore", Manifest: mustJSON(t, source)}, nil
	}
	_, m := h.run(t, restoreTask(t, "web"))
	if got := serviceOf(t, m, "web").Config[0].NotRestored; got != "was not restored by run #40; left in place" {
		t.Errorf("not restored = %q", got)
	}
}

func TestRestoreWithExpiredCopiesWarnsAndRestoresTaskDefinitionOnly(t *testing.T) {
	h := restoreHarness(t)
	delete(h.artifacts.items, sourceEnvKey) // retention removed the source run's previous copy

	out, m := h.run(t, restoreTask(t, "web"))
	if out.State != "succeeded" {
		t.Fatalf("State = %q (%s: %s), want succeeded: an expired copy is a warning, not a failure", out.State, out.FailureShort, out.FailureDetail)
	}
	web := serviceOf(t, m, "web")
	if web.TargetTaskDefinition != webOldTaskDef {
		t.Errorf("target task definition = %q, want the task definition still restored", web.TargetTaskDefinition)
	}
	const note = "config copy expired; only the task definition is restored"
	env := web.Config[0]
	if env.NotRestored != note || env.SHA256 != "" || env.ArtifactKey != "" || env.Change != "unchanged" {
		t.Errorf("env item = %+v, want it left in place", env)
	}
	c, ok := check(m, "web", "config copy web.env")
	if !ok || c.Status != "not_verified" || c.Detail != note {
		t.Errorf("check = %+v, %v, want not_verified with the reason", c, ok)
	}
	if len(m.Failed()) != 0 {
		t.Errorf("failed checks: %+v", m.Failed())
	}
	// The destination is still read and its current content kept for a later restore.
	if !env.PreviousExists || env.PreviousSHA256 != sha(envContent) || env.PreviousArtifactKey != currentEnvKey {
		t.Errorf("env item = %+v, want the current destination recorded", env)
	}
}

func TestRestoreWithCorruptCopyFailsTheCheck(t *testing.T) {
	h := restoreHarness(t)
	h.artifacts.items[sourceEnvKey] = []byte("not what was stored")

	out, m := h.run(t, restoreTask(t, "web"))
	c, ok := check(m, "web", "config copy web.env")
	if out.State != "failed" || !ok || c.Status != "failed" {
		t.Fatalf("outcome %q, check %+v (%v), want a failed leg and a failed check", out.State, c, ok)
	}
	if strings.Contains(c.Detail, "not what was stored") || strings.Contains(h.env.text(), "not what was stored") {
		t.Error("the content of the corrupt copy reached the check or the log")
	}
}

func TestRestoreFailsCheckWhenPreviousTaskDefinitionIsGone(t *testing.T) {
	cases := map[string]struct {
		setup  func(h *harness)
		detail string
	}{
		"deregistered and deleted": {
			setup:  func(h *harness) { delete(h.aws.taskDefs, webOldTaskDef) },
			detail: "the previous task definition " + webOldTaskDef + " no longer exists",
		},
		"inactive": {
			setup: func(h *harness) {
				td := h.aws.taskDefs[webOldTaskDef]
				td.TaskDefinition.Status = ecstypes.TaskDefinitionStatusInactive
				h.aws.taskDefs[webOldTaskDef] = td
			},
			detail: "the previous task definition " + webOldTaskDef + " is INACTIVE",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := restoreHarness(t)
			tc.setup(h)
			out, m := h.run(t, restoreTask(t, "web"))
			c, ok := check(m, "web", "task definition exists")
			if out.State != "failed" || !ok || c.Status != "failed" || c.Detail != tc.detail {
				t.Errorf("outcome %q, check %+v (%v), want a failed check %q", out.State, c, ok, tc.detail)
			}
		})
	}
}

func TestRestoreOfAServiceTheSourcePlanLacksFails(t *testing.T) {
	h := restoreHarness(t)
	out, _ := h.run(t, restoreTask(t, "web", "ghost"))
	if out.State != "failed" || !strings.Contains(out.FailureDetail, "ghost") {
		t.Errorf("outcome %q (%s), want a failure that names the service", out.State, out.FailureDetail)
	}
}

func TestRestoreRunIsStillResolvedAsADeployWhenItsKindIsDeploy(t *testing.T) {
	h := newHarness()
	out, m := h.run(t, claimed(t, "prod", defaultSelection()))
	if out.State != "succeeded" || m.RestoreOf != 0 || serviceOf(t, m, "web").TargetTaskDefinition != "" {
		t.Errorf("a deploy resolved to %q with restore_of %d", out.State, m.RestoreOf)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Result, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, present := decoded["restore_of"]; present {
		t.Error("a deploy's manifest carries restore_of")
	}
}
