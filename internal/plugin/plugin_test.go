package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeQuerier answers by query prefix and records the expanded queries.
type fakeQuerier struct {
	vals map[string]float64
	seen []string
	err  error
}

func (f *fakeQuerier) Query(_ context.Context, q string, _ time.Time) (float64, error) {
	f.seen = append(f.seen, q)
	if f.err != nil {
		return 0, f.err
	}
	for prefix, v := range f.vals {
		if strings.HasPrefix(q, prefix) {
			return v, nil
		}
	}
	return 0, nil
}

func metric(t *testing.T, cfg map[string]any) v1alpha1.Metric {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v1alpha1.Metric{Name: "regression", Provider: v1alpha1.MetricProvider{
		Plugin: map[string]json.RawMessage{Name: raw}}}
}

func baseConfig() map[string]any {
	return map[string]any{
		"address":  "http://prometheus:9090",
		"canary":   map[string]string{"bad": "cbad[$WINDOW]", "total": "ctotal[$WINDOW]"},
		"baseline": map[string]string{"bad": "bbad[$WINDOW]", "total": "btotal[$WINDOW]"},
	}
}

type harness struct {
	p   *RpcPlugin
	q   *fakeQuerier
	now time.Time
	run *v1alpha1.AnalysisRun
}

func newHarness(vals map[string]float64) *harness {
	h := &harness{q: &fakeQuerier{vals: vals}, now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	started := metav1.NewTime(h.now.Add(-60 * time.Second))
	h.run = &v1alpha1.AnalysisRun{Status: v1alpha1.AnalysisRunStatus{StartedAt: &started,
		MetricResults: []v1alpha1.MetricResult{{Name: "regression"}}}}
	h.p = &RpcPlugin{
		NewQuerier: func(string) (Querier, error) { return h.q, nil },
		Now:        func() time.Time { return h.now },
	}
	return h
}

// step runs one measurement, records it on the AnalysisRun and advances time.
func (h *harness) step(m v1alpha1.Metric) v1alpha1.Measurement {
	got := h.p.Run(h.run, m)
	h.run.Status.MetricResults[0].Measurements = append(h.run.Status.MetricResults[0].Measurements, got)
	h.now = h.now.Add(30 * time.Second)
	return got
}

func TestHealthyCanaryStaysSuccessful(t *testing.T) {
	h := newHarness(map[string]float64{"cbad": 1, "ctotal": 100, "bbad": 9, "btotal": 900})
	m := metric(t, baseConfig())
	for i := range 20 {
		got := h.step(m)
		if got.Phase != v1alpha1.AnalysisPhaseSuccessful {
			t.Fatalf("measurement %d: phase %s (%s), want Successful", i, got.Phase, got.Message)
		}
	}
	if h.q.seen[0] != "cbad[60s]" {
		t.Fatalf("first window should start at the analysis start, got %q", h.q.seen[0])
	}
	if last := h.q.seen[len(h.q.seen)-1]; last != "btotal[30s]" {
		t.Fatalf("later windows should start at the previous measurement, got %q", last)
	}
}

func TestRegressionFails(t *testing.T) {
	h := newHarness(map[string]float64{"cbad": 5, "ctotal": 100, "bbad": 9, "btotal": 900})
	m := metric(t, baseConfig())
	for range 20 {
		got := h.step(m)
		if got.Phase == v1alpha1.AnalysisPhaseFailed {
			if got.Metadata[MetaEValue] == "" || got.Metadata[MetaThreshold] != "20" {
				t.Fatalf("missing evidence metadata: %v", got.Metadata)
			}
			return
		}
	}
	t.Fatal("a 5x regression was never rejected")
}

func TestQueryErrorKeepsWindow(t *testing.T) {
	h := newHarness(map[string]float64{"cbad": 1, "ctotal": 100, "bbad": 9, "btotal": 900})
	m := metric(t, baseConfig())
	h.step(m)
	h.q.err = errors.New("prometheus down")
	if got := h.step(m); got.Phase != v1alpha1.AnalysisPhaseError {
		t.Fatalf("phase %s, want Error", got.Phase)
	}
	h.q.err = nil
	h.q.seen = nil
	if got := h.step(m); got.Phase != v1alpha1.AnalysisPhaseSuccessful {
		t.Fatalf("phase %s after recovery", got.Phase)
	}
	if h.q.seen[0] != "cbad[60s]" {
		t.Fatalf("window after an error should cover both intervals, got %q", h.q.seen[0])
	}
}

func TestChangingParametersMidAnalysisIsAnError(t *testing.T) {
	h := newHarness(map[string]float64{"cbad": 1, "ctotal": 100, "bbad": 9, "btotal": 900})
	h.step(metric(t, baseConfig()))
	cfg := baseConfig()
	cfg["margin"] = 0.3
	if got := h.step(metric(t, cfg)); got.Phase != v1alpha1.AnalysisPhaseError {
		t.Fatalf("phase %s, want Error", got.Phase)
	}
}

func TestConfigValidation(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"no address":   func(c map[string]any) { delete(c, "address") },
		"no query":     func(c map[string]any) { c["canary"] = map[string]string{"bad": "x"} },
		"bad alpha":    func(c map[string]any) { c["alpha"] = 1.5 },
		"bad maxRatio": func(c map[string]any) { c["maxRatio"] = 1.01 },
	} {
		cfg := baseConfig()
		mutate(cfg)
		h := newHarness(nil)
		if got := h.step(metric(t, cfg)); got.Phase != v1alpha1.AnalysisPhaseError {
			t.Errorf("%s: phase %s, want Error", name, got.Phase)
		}
	}
	zero := baseConfig()
	zero["margin"] = 0
	if _, p, err := parseConfig(metric(t, zero)); err != nil || p.Margin != 0 {
		t.Fatalf("an explicit margin of 0 must be kept: %v %+v", err, p)
	}
}

func TestPromDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "1s", 1500 * time.Millisecond: "2s", time.Minute: "60s"} {
		if got := promDuration(d); got != want {
			t.Errorf("promDuration(%v) = %s, want %s", d, got, want)
		}
	}
}
