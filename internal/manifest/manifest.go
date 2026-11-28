// Package manifest defines the deployment manifest a plan pins, its identity
// hash and the env-file key diff. It is pure: no I/O, no logging.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Manifest is everything a plan resolved and pinned for one deployment.
type Manifest struct {
	ID              string    `json:"id"` // sha256 hex over the pinned inputs; see Identity
	RunID           int64     `json:"run_id"`
	Environment     string    `json:"environment"`
	Label           string    `json:"label"`
	AWSAccount      string    `json:"aws_account"`
	InventoryCommit string    `json:"inventory_commit"`
	JobCommit       string    `json:"job_commit"`
	RestoreOf       int64     `json:"restore_of,omitempty"` // source run id of a restore run; 0 for a deploy
	Services        []Service `json:"services"`             // sorted by name
	Checks          []Check   `json:"checks"`
}

// Service is one service to deploy with the config it carries.
type Service struct {
	Name                   string       `json:"name"`
	Group                  string       `json:"group"`
	Version                string       `json:"version"`
	ImageDigest            string       `json:"image_digest"`
	ECSService             string       `json:"ecs_service"`
	ECRRepository          string       `json:"ecr_repository"`
	PreviousTaskDefinition string       `json:"previous_task_definition"`         // full ARN with revision
	PreviousVersion        string       `json:"previous_version"`                 // "" if unknown
	TargetTaskDefinition   string       `json:"target_task_definition,omitempty"` // restore only: exact task definition ARN to put the service on; "" for a deploy
	Config                 []ConfigItem `json:"config"`                           // env files then TOMLs, in inventory order
}

// ConfigItem is one config file pinned for a service.
type ConfigItem struct {
	Kind                string   `json:"kind"` // "env" | "toml"
	Name                string   `json:"name"`
	Source              string   `json:"source"`
	Destination         string   `json:"destination"`
	Branch              string   `json:"branch,omitempty"` // env only
	Commit              string   `json:"commit,omitempty"` // env only
	Fallback            bool     `json:"fallback,omitempty"`
	SourceVersionID     string   `json:"source_version_id,omitempty"` // toml only, when the bucket has versioning
	SHA256              string   `json:"sha256"`
	ArtifactKey         string   `json:"artifact_key,omitempty"`
	Size                int64    `json:"size"`
	PreviousExists      bool     `json:"previous_exists"`
	PreviousSHA256      string   `json:"previous_sha256,omitempty"`
	PreviousArtifactKey string   `json:"previous_artifact_key,omitempty"`
	Change              string   `json:"change"`                 // "new" | "changed" | "unchanged" | "unknown" (a failed plan could not tell)
	Keys                *KeyDiff `json:"keys,omitempty"`         // env only, when changed
	NotRestored         string   `json:"not_restored,omitempty"` // restore only: why this file is left as it is; "" otherwise
}

// KeyDiff lists env variable names, never values, sorted.
type KeyDiff struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
}

// Check is one preflight result. Status: "passed" | "failed" | "not_verified".
type Check struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
}

// Identity returns the sha256 hex of a canonical JSON encoding of the pinned
// inputs, so two plans of the same inputs have the same identity.
//
// Included: environment, AWS account, inventory commit, job commit; per service
// (sorted by name) name, version, image digest, ECS service ARN, previous task
// definition; per config item (in the given order, which is inventory order)
// kind, name, destination, SHA-256; target task definition (restore only) and
// not-restored reason (restore only), which are omitted when empty so a deploy
// keeps its identity.
//
// Excluded: the manifest ID and run ID, label, checks, group, previous version,
// ECR repository, artifact keys, change and key-diff fields, sizes and sources.
// The encoding is fixed by the struct field order below; do not reorder or
// rename fields without a deliberate identity-version change.
func Identity(m Manifest) string {
	type configInput struct {
		Kind        string `json:"kind"`
		Name        string `json:"name"`
		Destination string `json:"destination"`
		SHA256      string `json:"sha256"`
		NotRestored string `json:"not_restored,omitempty"`
	}
	type serviceInput struct {
		Name                   string        `json:"name"`
		Version                string        `json:"version"`
		ImageDigest            string        `json:"image_digest"`
		ECSService             string        `json:"ecs_service"`
		PreviousTaskDefinition string        `json:"previous_task_definition"`
		Config                 []configInput `json:"config"`
		TargetTaskDefinition   string        `json:"target_task_definition,omitempty"`
	}
	type input struct {
		Scheme          string         `json:"scheme"`
		Environment     string         `json:"environment"`
		AWSAccount      string         `json:"aws_account"`
		InventoryCommit string         `json:"inventory_commit"`
		JobCommit       string         `json:"job_commit"`
		Services        []serviceInput `json:"services"`
	}

	canonical := input{
		Scheme:          "yumdrake-manifest-v1",
		Environment:     m.Environment,
		AWSAccount:      m.AWSAccount,
		InventoryCommit: m.InventoryCommit,
		JobCommit:       m.JobCommit,
		Services:        make([]serviceInput, 0, len(m.Services)),
	}
	for _, s := range m.Services {
		si := serviceInput{
			Name: s.Name, Version: s.Version, ImageDigest: s.ImageDigest,
			ECSService: s.ECSService, PreviousTaskDefinition: s.PreviousTaskDefinition,
			Config: make([]configInput, 0, len(s.Config)), TargetTaskDefinition: s.TargetTaskDefinition,
		}
		for _, c := range s.Config {
			si.Config = append(si.Config, configInput{Kind: c.Kind, Name: c.Name, Destination: c.Destination, SHA256: c.SHA256, NotRestored: c.NotRestored})
		}
		canonical.Services = append(canonical.Services, si)
	}
	sort.SliceStable(canonical.Services, func(i, j int) bool {
		a, b := canonical.Services[i], canonical.Services[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ECSService < b.ECSService
	})

	// Marshaling plain strings and slices cannot fail.
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Failed returns the checks whose status is "failed".
func (m Manifest) Failed() []Check {
	var failed []Check
	for _, c := range m.Checks {
		if c.Status == "failed" {
			failed = append(failed, c)
		}
	}
	return failed
}

// Redacted returns a copy without artifact keys; this is what the API serves.
// The receiver is not modified.
func (m Manifest) Redacted() Manifest {
	out := m
	out.Services = make([]Service, len(m.Services))
	for i, s := range m.Services {
		s.Config = append([]ConfigItem(nil), s.Config...)
		for j := range s.Config {
			s.Config[j].ArtifactKey = ""
			s.Config[j].PreviousArtifactKey = ""
		}
		out.Services[i] = s
	}
	return out
}
