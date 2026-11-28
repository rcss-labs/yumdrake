// Package metrics writes the orchestrator's CloudWatch metrics in embedded
// metric format and collects their values from the database.
package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"
)

const (
	// maxDimensions is CloudWatch's limit of dimension keys per metric.
	maxDimensions = 30
	// maxValuesPerMetric is CloudWatch's limit of values in one EMF metric array.
	maxValuesPerMetric = 100
)

// Sample is one value of one metric. Several samples with the same Name in one
// Write become one metric with several values (a distribution).
type Sample struct {
	Name  string
	Value float64
	Unit  string
}

type metricDefinition struct {
	Name string `json:"Name"`
	Unit string `json:"Unit,omitempty"`
}

type metricDirective struct {
	Namespace  string             `json:"Namespace"`
	Dimensions [][]string         `json:"Dimensions"`
	Metrics    []metricDefinition `json:"Metrics"`
}

type awsMetadata struct {
	Timestamp         int64             `json:"Timestamp"`
	CloudWatchMetrics []metricDirective `json:"CloudWatchMetrics"`
}

// Write writes samples as exactly one line of CloudWatch embedded metric
// format: a JSON object with the "_aws" directive, each dimension as a string
// member and each metric as a number (or an array when it has several values).
// at is the time of the values, in Unix milliseconds. Nothing is written when
// the input is not valid: no samples, a sample name that is empty, "_aws" or a
// dimension name, a dimension with an empty name or value, more than 30
// dimensions, more than 100 values for one name, or a NaN or infinite value.
func Write(w io.Writer, namespace string, dims map[string]string, at time.Time, samples []Sample) error {
	if len(samples) == 0 {
		return errors.New("metrics: no samples")
	}
	if len(dims) > maxDimensions {
		return fmt.Errorf("metrics: %d dimensions, at most %d are allowed", len(dims), maxDimensions)
	}
	dimNames := make([]string, 0, len(dims))
	for name, value := range dims {
		if name == "" || value == "" {
			return errors.New("metrics: a dimension needs a name and a value")
		}
		dimNames = append(dimNames, name)
	}
	sort.Strings(dimNames)

	var order []string
	units := map[string]string{}
	values := map[string][]float64{}
	for _, s := range samples {
		switch {
		case s.Name == "" || s.Name == "_aws":
			return fmt.Errorf("metrics: %q is not a valid metric name", s.Name)
		case dims != nil && hasKey(dims, s.Name):
			return fmt.Errorf("metrics: metric %q has the name of a dimension", s.Name)
		case math.IsNaN(s.Value) || math.IsInf(s.Value, 0):
			return fmt.Errorf("metrics: metric %q has a value that is not a finite number", s.Name)
		}
		if _, seen := values[s.Name]; !seen {
			order = append(order, s.Name)
			units[s.Name] = s.Unit
		}
		values[s.Name] = append(values[s.Name], s.Value)
		if len(values[s.Name]) > maxValuesPerMetric {
			return fmt.Errorf("metrics: metric %q has more than %d values", s.Name, maxValuesPerMetric)
		}
	}

	definitions := make([]metricDefinition, 0, len(order))
	for _, name := range order {
		definitions = append(definitions, metricDefinition{Name: name, Unit: units[name]})
	}
	directive := awsMetadata{
		Timestamp: at.UnixMilli(),
		CloudWatchMetrics: []metricDirective{{
			Namespace: namespace, Dimensions: [][]string{append([]string{}, dimNames...)}, Metrics: definitions,
		}},
	}

	var line bytes.Buffer
	line.WriteString(`{"_aws":`)
	if err := appendJSON(&line, directive); err != nil {
		return err
	}
	for _, name := range dimNames {
		if err := appendMember(&line, name, dims[name]); err != nil {
			return err
		}
	}
	for _, name := range order {
		var v any = values[name][0]
		if len(values[name]) > 1 {
			v = values[name]
		}
		if err := appendMember(&line, name, v); err != nil {
			return err
		}
	}
	line.WriteString("}\n")
	// One Write call, so the line is not interleaved with other output.
	_, err := w.Write(line.Bytes())
	return err
}

func hasKey(m map[string]string, key string) bool {
	_, ok := m[key]
	return ok
}

func appendJSON(b *bytes.Buffer, v any) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	b.Write(encoded)
	return nil
}

func appendMember(b *bytes.Buffer, name string, v any) error {
	b.WriteByte(',')
	if err := appendJSON(b, name); err != nil {
		return err
	}
	b.WriteByte(':')
	return appendJSON(b, v)
}
