package metrics

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestWriteProducesValidEMF(t *testing.T) {
	at := time.Date(2026, 11, 28, 12, 0, 0, 123_456_789, time.UTC)
	samples := []Sample{
		{Name: "QueueDepth", Value: 3, Unit: "Count"},
		{Name: "RunDuration", Value: 12.5, Unit: "Seconds"},
		{Name: "RunDuration", Value: 40, Unit: "Seconds"},
		{Name: "OldestQueuedLegAge", Value: 0, Unit: "Seconds"},
	}
	var out bytes.Buffer
	if err := Write(&out, "Yumdrake", map[string]string{"Environment": "prod", "Cluster": "main"}, at, samples); err != nil {
		t.Fatalf("Write: %v", err)
	}
	line := out.String()
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("output %q is not exactly one line", line)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, line)
	}
	var aws struct {
		Timestamp         int64
		CloudWatchMetrics []struct {
			Namespace  string
			Dimensions [][]string
			Metrics    []struct{ Name, Unit string }
		}
	}
	if err := json.Unmarshal(doc["_aws"], &aws); err != nil {
		t.Fatalf("_aws: %v", err)
	}
	if aws.Timestamp != at.UnixMilli() {
		t.Errorf("Timestamp = %d, want %d (Unix milliseconds)", aws.Timestamp, at.UnixMilli())
	}
	if len(aws.CloudWatchMetrics) != 1 {
		t.Fatalf("CloudWatchMetrics has %d entries, want 1", len(aws.CloudWatchMetrics))
	}
	d := aws.CloudWatchMetrics[0]
	if d.Namespace != "Yumdrake" {
		t.Errorf("Namespace = %q, want Yumdrake", d.Namespace)
	}
	if len(d.Dimensions) != 1 || strings.Join(d.Dimensions[0], ",") != "Cluster,Environment" {
		t.Errorf("Dimensions = %v, want one set with the sorted names Cluster, Environment", d.Dimensions)
	}
	var names []string
	for _, m := range d.Metrics {
		names = append(names, m.Name+"/"+m.Unit)
	}
	if got := strings.Join(names, " "); got != "QueueDepth/Count RunDuration/Seconds OldestQueuedLegAge/Seconds" {
		t.Errorf("Metrics = %q, want each name once in first-seen order with its unit", got)
	}

	// Every name the directive lists has its value at the top level.
	for _, want := range []struct{ key, json string }{
		{"Environment", `"prod"`}, {"Cluster", `"main"`}, {"QueueDepth", `3`},
		{"OldestQueuedLegAge", `0`}, {"RunDuration", `[12.5,40]`},
	} {
		if got := strings.ReplaceAll(string(doc[want.key]), " ", ""); got != want.json {
			t.Errorf("%s = %s, want %s", want.key, got, want.json)
		}
	}
}

func TestWriteWithoutDimensionsHasOneEmptySet(t *testing.T) {
	var out bytes.Buffer
	if err := Write(&out, "Yumdrake", nil, time.Unix(1, 0), []Sample{{Name: "ActiveRunners", Value: 2, Unit: "Count"}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(out.String(), `"Dimensions":[[]]`) {
		t.Fatalf("output %s, want \"Dimensions\":[[]]", out.String())
	}
}

func TestWriteRefusesBadInput(t *testing.T) {
	ok := []Sample{{Name: "QueueDepth", Value: 1, Unit: "Count"}}
	many := make([]Sample, 101)
	for i := range many {
		many[i] = Sample{Name: "RunDuration", Value: float64(i), Unit: "Seconds"}
	}
	thirtyOne := map[string]string{}
	for i := range 31 {
		thirtyOne[strings.Repeat("d", i+1)] = "v"
	}
	thirty := map[string]string{}
	for i := range 30 {
		thirty[strings.Repeat("d", i+1)] = "v"
	}
	cases := []struct {
		name    string
		dims    map[string]string
		samples []Sample
		wantErr bool
	}{
		{"no samples", nil, nil, true},
		{"empty sample name", nil, []Sample{{Name: "", Value: 1, Unit: "Count"}}, true},
		{"sample named like a dimension", map[string]string{"QueueDepth": "x"}, ok, true},
		{"sample named _aws", nil, []Sample{{Name: "_aws", Value: 1, Unit: "Count"}}, true},
		{"31 dimensions", thirtyOne, ok, true},
		{"101 values for one name", nil, many, true},
		{"NaN", nil, []Sample{{Name: "A", Value: math.NaN(), Unit: "Count"}}, true},
		{"positive infinity", nil, []Sample{{Name: "A", Value: math.Inf(1), Unit: "Count"}}, true},
		{"negative infinity", nil, []Sample{{Name: "A", Value: math.Inf(-1), Unit: "Count"}}, true},
		{"empty dimension value", map[string]string{"Environment": ""}, ok, true},
		{"30 dimensions are fine", thirty, ok, false},
		{"100 values for one name are fine", nil, many[:100], false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := Write(&out, "Yumdrake", c.dims, time.Unix(1, 0), c.samples)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Write returned no error; output %q", out.String())
				}
				if out.Len() != 0 {
					t.Fatalf("Write failed but wrote %q", out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if out.Len() == 0 {
				t.Fatal("Write wrote nothing")
			}
		})
	}
}

func TestEmitterSplitsMoreThan100Durations(t *testing.T) {
	samples := []Sample{{Name: "QueueDepth", Value: 1, Unit: "Count"}}
	for i := range 250 {
		samples = append(samples, Sample{Name: "RunDuration", Value: float64(i), Unit: "Seconds"})
	}
	samples = append(samples, Sample{Name: "SyncAge", Value: 5, Unit: "Seconds"})

	lines := splitSamples(samples)
	if len(lines) != 3 {
		t.Fatalf("splitSamples gave %d lines, want 3", len(lines))
	}
	total := 0
	for i, line := range lines {
		var out bytes.Buffer
		if err := Write(&out, "Yumdrake", map[string]string{"E": "p"}, time.Unix(1, 0), line); err != nil {
			t.Fatalf("line %d is not writable: %v", i, err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		var durations []float64
		if err := json.Unmarshal(doc["RunDuration"], &durations); err != nil {
			t.Fatalf("line %d RunDuration: %v", i, err)
		}
		total += len(durations)
		_, hasQueue := doc["QueueDepth"]
		_, hasSync := doc["SyncAge"]
		if (i == 0) != (hasQueue && hasSync) || (i > 0 && (hasQueue || hasSync)) {
			t.Errorf("line %d: QueueDepth %v, SyncAge %v; the other metrics belong only in the first line", i, hasQueue, hasSync)
		}
	}
	if total != 250 {
		t.Errorf("durations over all lines = %d, want 250", total)
	}
	if n := len(splitSamples(samples[:3])); n != 1 {
		t.Errorf("few samples gave %d lines, want 1", n)
	}
}
