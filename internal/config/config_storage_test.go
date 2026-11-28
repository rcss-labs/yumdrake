package config

import (
	"strings"
	"testing"
)

func TestRunnerArtifactDir(t *testing.T) {
	t.Run("unset is empty", func(t *testing.T) {
		cfg, err := LoadRunner(envFrom(requiredEnv()))
		if err != nil {
			t.Fatalf("LoadRunner: %v", err)
		}
		if cfg.ArtifactDir != "" {
			t.Errorf("ArtifactDir = %q, want empty", cfg.ArtifactDir)
		}
	})

	t.Run("absolute path is carried over", func(t *testing.T) {
		env := requiredEnv()
		delete(env, "YUMDRAKE_ARTIFACT_BUCKET")
		env["YUMDRAKE_ARTIFACT_DIR"] = "/var/lib/yumdrake/artifacts"
		cfg, err := LoadRunner(envFrom(env))
		if err != nil {
			t.Fatalf("LoadRunner: %v", err)
		}
		if cfg.ArtifactDir != "/var/lib/yumdrake/artifacts" {
			t.Errorf("ArtifactDir = %q", cfg.ArtifactDir)
		}
	})

	t.Run("relative path is refused", func(t *testing.T) {
		env := requiredEnv()
		delete(env, "YUMDRAKE_ARTIFACT_BUCKET")
		env["YUMDRAKE_ARTIFACT_DIR"] = "relative/dir"
		_, err := LoadRunner(envFrom(env))
		if err == nil || !strings.Contains(err.Error(), "YUMDRAKE_ARTIFACT_DIR") {
			t.Errorf("err = %v, want one naming YUMDRAKE_ARTIFACT_DIR", err)
		}
	})

	t.Run("bucket and directory together are refused", func(t *testing.T) {
		env := requiredEnv()
		env["YUMDRAKE_ARTIFACT_DIR"] = "/var/lib/yumdrake/artifacts"
		_, err := LoadRunner(envFrom(env))
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("err = %v, want a mutually exclusive error", err)
		}
	})
}
