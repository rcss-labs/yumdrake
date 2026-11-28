package config

import (
	"reflect"
	"strings"
	"testing"
)

func metricsEnv(extra map[string]string) func(string) string {
	env := requiredEnv()
	for k, v := range extra {
		env[k] = v
	}
	return envFrom(env)
}

func TestLoadOrchestratorMetricsDefaultsOff(t *testing.T) {
	cfg, err := LoadOrchestrator(envFrom(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadOrchestrator() error = %v", err)
	}
	if cfg.Metrics != "" || len(cfg.MetricsDimensions) != 0 {
		t.Errorf("Metrics = %q, MetricsDimensions = %v; want off and none", cfg.Metrics, cfg.MetricsDimensions)
	}
}

func TestLoadOrchestratorMetricsEMFWithDimensions(t *testing.T) {
	cfg, err := LoadOrchestrator(metricsEnv(map[string]string{
		"YUMDRAKE_METRICS":            "emf",
		"YUMDRAKE_METRICS_DIMENSIONS": "Environment=prod,Service.Name=yumdrake-api",
	}))
	if err != nil {
		t.Fatalf("LoadOrchestrator() error = %v", err)
	}
	want := map[string]string{"Environment": "prod", "Service.Name": "yumdrake-api"}
	if cfg.Metrics != "emf" || !reflect.DeepEqual(cfg.MetricsDimensions, want) {
		t.Errorf("Metrics = %q, MetricsDimensions = %v; want emf and %v", cfg.Metrics, cfg.MetricsDimensions, want)
	}
}

func TestLoadOrchestratorMetricsEmptyDimensionsMeansNone(t *testing.T) {
	// Terraform joins an empty map to an empty string.
	cfg, err := LoadOrchestrator(metricsEnv(map[string]string{"YUMDRAKE_METRICS": "emf", "YUMDRAKE_METRICS_DIMENSIONS": ""}))
	if err != nil || len(cfg.MetricsDimensions) != 0 {
		t.Fatalf("LoadOrchestrator() = %v, %v; want no dimensions", cfg.MetricsDimensions, err)
	}
}

func TestLoadOrchestratorRejectsBadMetricsSettings(t *testing.T) {
	thirtyOne := make([]string, 31)
	for i := range thirtyOne {
		thirtyOne[i] = "d" + strings.Repeat("x", i) + "=v"
	}
	cases := []struct {
		name, variable, value, wantMessage string
	}{
		{"unknown mode", "YUMDRAKE_METRICS", "prometheus", `YUMDRAKE_METRICS: must be "" or "emf"`},
		{"mode is case sensitive", "YUMDRAKE_METRICS", "EMF", `YUMDRAKE_METRICS: must be "" or "emf"`},
		{"pair without value", "YUMDRAKE_METRICS_DIMENSIONS", "Environment", "YUMDRAKE_METRICS_DIMENSIONS"},
		{"empty value", "YUMDRAKE_METRICS_DIMENSIONS", "Environment=", "YUMDRAKE_METRICS_DIMENSIONS"},
		{"empty name", "YUMDRAKE_METRICS_DIMENSIONS", "=prod", "YUMDRAKE_METRICS_DIMENSIONS"},
		{"bad name characters", "YUMDRAKE_METRICS_DIMENSIONS", "Env ironment=prod", "YUMDRAKE_METRICS_DIMENSIONS"},
		{"name too long", "YUMDRAKE_METRICS_DIMENSIONS", strings.Repeat("a", 256) + "=v", "YUMDRAKE_METRICS_DIMENSIONS"},
		{"duplicate name", "YUMDRAKE_METRICS_DIMENSIONS", "a=1,a=2", "YUMDRAKE_METRICS_DIMENSIONS"},
		{"trailing comma", "YUMDRAKE_METRICS_DIMENSIONS", "a=1,", "YUMDRAKE_METRICS_DIMENSIONS"},
		{"named like the EMF directive", "YUMDRAKE_METRICS_DIMENSIONS", "_aws=x", "YUMDRAKE_METRICS_DIMENSIONS"},
		{"named like a metric", "YUMDRAKE_METRICS_DIMENSIONS", "QueueDepth=x", "YUMDRAKE_METRICS_DIMENSIONS"},
		{"too many", "YUMDRAKE_METRICS_DIMENSIONS", strings.Join(thirtyOne, ","), "YUMDRAKE_METRICS_DIMENSIONS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadOrchestrator(metricsEnv(map[string]string{c.variable: c.value}))
			if err == nil || !strings.Contains(err.Error(), c.wantMessage) {
				t.Fatalf("LoadOrchestrator() error = %v, want one mentioning %q", err, c.wantMessage)
			}
		})
	}
}
