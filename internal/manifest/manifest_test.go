package manifest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func sampleManifest() Manifest {
	return Manifest{
		RunID:           7,
		Environment:     "prod",
		Label:           "Production",
		AWSAccount:      "123456789012",
		InventoryCommit: "inv111",
		JobCommit:       "job222",
		Services: []Service{
			{
				Name: "web", Group: "g1", Version: "1.2.3", ImageDigest: "sha256:aaa",
				ECSService: "arn:aws:ecs:eu-west-1:123456789012:service/c/web", ECRRepository: "web",
				PreviousTaskDefinition: "arn:aws:ecs:eu-west-1:123456789012:task-definition/web:4",
				Config: []ConfigItem{
					{Kind: "env", Name: "app", Destination: "s3://b/app.env", SHA256: "e1", ArtifactKey: "art/e1", PreviousArtifactKey: "art/e0", Change: "changed"},
					{Kind: "toml", Name: "cfg", Destination: "s3://b/cfg.toml", SHA256: "t1", ArtifactKey: "art/t1"},
				},
			},
			{
				Name: "api", Group: "g1", Version: "2.0.0", ImageDigest: "sha256:bbb",
				ECSService:             "arn:aws:ecs:eu-west-1:123456789012:service/c/api",
				PreviousTaskDefinition: "arn:aws:ecs:eu-west-1:123456789012:task-definition/api:9",
			},
		},
		Checks: []Check{
			{Name: "digest", Service: "web", Status: "passed"},
			{Name: "tags", Service: "api", Status: "failed", Detail: "missing"},
			{Name: "kms", Service: "api", Status: "not_verified"},
		},
	}
}

func TestIdentityStableAcrossOrderingAndRuns(t *testing.T) {
	a := sampleManifest()
	b := sampleManifest()
	b.Services[0], b.Services[1] = b.Services[1], b.Services[0]

	first := Identity(a)
	if len(first) != 64 {
		t.Fatalf("identity %q is not a sha256 hex string", first)
	}
	for i := 0; i < 5; i++ {
		if got := Identity(a); got != first {
			t.Fatalf("run %d: identity changed: %s != %s", i, got, first)
		}
	}
	if got := Identity(b); got != first {
		t.Fatalf("service order changed identity: %s != %s", got, first)
	}
	if a.Services[0].Name != "web" {
		t.Fatal("Identity must not reorder the caller's services")
	}
}

func TestIdentityChangesWithDigestCommitOrChecksum(t *testing.T) {
	base := Identity(sampleManifest())
	cases := map[string]func(m *Manifest){
		"image digest":       func(m *Manifest) { m.Services[0].ImageDigest = "sha256:zzz" },
		"version":            func(m *Manifest) { m.Services[0].Version = "9.9.9" },
		"inventory commit":   func(m *Manifest) { m.InventoryCommit = "other" },
		"job commit":         func(m *Manifest) { m.JobCommit = "other" },
		"environment":        func(m *Manifest) { m.Environment = "stage" },
		"account":            func(m *Manifest) { m.AWSAccount = "999999999999" },
		"ecs service":        func(m *Manifest) { m.Services[1].ECSService = "other" },
		"previous task def":  func(m *Manifest) { m.Services[1].PreviousTaskDefinition = "other:1" },
		"config checksum":    func(m *Manifest) { m.Services[0].Config[0].SHA256 = "changed" },
		"config destination": func(m *Manifest) { m.Services[0].Config[1].Destination = "s3://b/x.toml" },
		"config name":        func(m *Manifest) { m.Services[0].Config[1].Name = "other" },
		"config kind":        func(m *Manifest) { m.Services[0].Config[1].Kind = "env" },
		"service name":       func(m *Manifest) { m.Services[1].Name = "zzz" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := sampleManifest()
			mutate(&m)
			if Identity(m) == base {
				t.Fatalf("changing %s did not change the identity", name)
			}
		})
	}
}

func TestIdentityIgnoresChecksAndArtifactKeys(t *testing.T) {
	base := Identity(sampleManifest())
	m := sampleManifest()
	m.RunID = 99
	m.Label = "renamed"
	m.Checks = nil
	m.Services[0].Config[0].ArtifactKey = "different"
	m.Services[0].Config[0].PreviousArtifactKey = ""
	m.Services[0].Config[0].Change = "new"
	m.Services[0].Config[0].Keys = &KeyDiff{Added: []string{"A"}}
	m.Services[0].PreviousVersion = "0.0.1"
	m.ID = "something"
	if got := Identity(m); got != base {
		t.Fatalf("identity moved on ignored fields: %s != %s", got, base)
	}
}

func TestRedactedDropsArtifactKeys(t *testing.T) {
	m := sampleManifest()
	r := m.Redacted()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"art/e1", "art/e0", "art/t1"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("redacted manifest still contains %q", secret)
		}
	}
	if r.Services[0].Config[0].SHA256 != "e1" || r.Services[0].Config[0].Name != "app" {
		t.Fatal("redaction removed fields other than artifact keys")
	}
	if m.Services[0].Config[0].ArtifactKey != "art/e1" || m.Services[0].Config[0].PreviousArtifactKey != "art/e0" {
		t.Fatal("Redacted mutated the original manifest")
	}
}

func TestFailed(t *testing.T) {
	failed := sampleManifest().Failed()
	if len(failed) != 1 || failed[0].Name != "tags" {
		t.Fatalf("Failed() = %+v, want only the failed check", failed)
	}
}

// Pinned before the restore fields existed: a deploy manifest's identity must not move.
const deployIdentityBeforeRestoreFields = "99d475132efe5bbd64bb3ae2e28d02a9c66c909ea0390738ba71f73b3dc6148a"

func TestIdentityOfADeployManifestIsUnchanged(t *testing.T) {
	if got := Identity(sampleManifest()); got != deployIdentityBeforeRestoreFields {
		t.Fatalf("deploy identity changed: got %s, want %s", got, deployIdentityBeforeRestoreFields)
	}
}

func TestIdentityChangesWithTargetTaskDefinition(t *testing.T) {
	unset := Identity(sampleManifest())
	first := sampleManifest()
	first.Services[0].TargetTaskDefinition = "arn:aws:ecs:eu-west-1:123456789012:task-definition/web:3"
	second := sampleManifest()
	second.Services[0].TargetTaskDefinition = "arn:aws:ecs:eu-west-1:123456789012:task-definition/web:2"

	firstID, secondID := Identity(first), Identity(second)
	if firstID == secondID {
		t.Fatal("two different target task definitions gave the same identity")
	}
	if firstID == unset || secondID == unset {
		t.Fatal("a set target task definition gave the same identity as an unset one")
	}
}

func TestIdentityChangesWithNotRestored(t *testing.T) {
	base := Identity(sampleManifest())
	m := sampleManifest()
	m.Services[0].Config[0].NotRestored = "file changed since the source run"
	if Identity(m) == base {
		t.Fatal("setting not_restored did not change the identity")
	}
}

func TestRestoreFieldsRoundTripJSON(t *testing.T) {
	restore := sampleManifest()
	restore.RestoreOf = 5
	restore.Services[0].TargetTaskDefinition = "arn:aws:ecs:eu-west-1:123456789012:task-definition/web:3"
	restore.Services[0].Config[0].NotRestored = "file changed since the source run"

	raw, err := json.Marshal(restore)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"restore_of":`, `"target_task_definition":`, `"not_restored":`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("marshaled restore manifest lacks %s: %s", key, raw)
		}
	}
	var back Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, restore) {
		t.Fatalf("round trip changed the manifest:\n got %+v\nwant %+v", back, restore)
	}

	deploy, err := json.Marshal(sampleManifest())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"restore_of", "target_task_definition", "not_restored"} {
		if strings.Contains(string(deploy), key) {
			t.Fatalf("deploy manifest JSON contains %s: %s", key, deploy)
		}
	}
}
