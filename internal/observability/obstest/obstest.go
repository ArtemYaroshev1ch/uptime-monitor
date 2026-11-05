// Package obstest reads metric values from a registry in tests. The metrics
// of a component are unexported fields, so tests look them up by name the way
// Prometheus would.
package obstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// Value returns the value of the series name{labels} in reg: a counter or
// gauge value, or for a histogram the number of observations. labels are
// name/value pairs. The test fails if the series does not exist.
func Value(t testing.TB, reg prometheus.Gatherer, name string, labels ...string) float64 {
	t.Helper()
	m := find(t, reg, name, labels)
	switch {
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	case m.GetHistogram() != nil:
		return float64(m.GetHistogram().GetSampleCount())
	}
	t.Fatalf("metric %s has an unsupported type", name)
	return 0
}

// Sum returns the sum of the observations of the histogram name{labels}.
func Sum(t testing.TB, reg prometheus.Gatherer, name string, labels ...string) float64 {
	t.Helper()
	h := find(t, reg, name, labels).GetHistogram()
	require.NotNil(t, h, "%s is not a histogram", name)
	return h.GetSampleSum()
}

func find(t testing.TB, reg prometheus.Gatherer, name string, labels []string) *dto.Metric {
	t.Helper()
	require.Zero(t, len(labels)%2, "labels must be name/value pairs")
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if hasLabels(m, labels) {
				return m
			}
		}
	}
	t.Fatalf("no series %s%v", name, labels)
	return nil
}

func hasLabels(m *dto.Metric, want []string) bool {
	if len(m.GetLabel()) != len(want)/2 {
		return false
	}
	have := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		have[l.GetName()] = l.GetValue()
	}
	for i := 0; i < len(want); i += 2 {
		if v, ok := have[want[i]]; !ok || v != want[i+1] {
			return false
		}
	}
	return true
}
