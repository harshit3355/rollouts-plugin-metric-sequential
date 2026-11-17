// Package plugin implements the Argo Rollouts metric provider. Each Run
// queries the bad and total event counts for the canary and the baseline over
// the window since the previous measurement, adds them to the sequential test
// and stores the test's state in the measurement's metadata for the next Run.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/argoproj/argo-rollouts/metricproviders/plugin"
	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	metricutil "github.com/argoproj/argo-rollouts/utils/metric"
	"github.com/argoproj/argo-rollouts/utils/plugin/types"
	timeutil "github.com/argoproj/argo-rollouts/utils/time"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/harshit3355/rollouts-plugin-metric-sequential/internal/seqtest"
)

// Name is the key under spec.metrics[].provider.plugin and in the
// argo-rollouts-config ConfigMap.
const Name = "harshit3355/sequential"

// WindowPlaceholder is replaced in every query with the window since the
// previous measurement, for example "60s".
const WindowPlaceholder = "$WINDOW"

// Metadata keys written on each measurement.
const (
	MetaState     = "state"
	MetaWindowEnd = "windowEnd"
	MetaEValue    = "eValue"
	MetaThreshold = "rejectAt"
	MetaWindow    = "window"
	MetaCounts    = "windowCounts"
)

// Query is a pair of PromQL expressions for one arm.
type Query struct {
	Bad   string `json:"bad"`
	Total string `json:"total"`
}

// Config is the plugin block of an AnalysisTemplate metric.
type Config struct {
	Address  string   `json:"address"`
	Canary   Query    `json:"canary"`
	Baseline Query    `json:"baseline"`
	Alpha    float64  `json:"alpha,omitempty"`
	Margin   *float64 `json:"margin,omitempty"`
	MaxRatio float64  `json:"maxRatio,omitempty"`
	Points   int      `json:"points,omitempty"`
}

func parseConfig(metric v1alpha1.Metric) (Config, seqtest.Params, error) {
	var c Config
	raw, ok := metric.Provider.Plugin[Name]
	if !ok {
		return c, seqtest.Params{}, fmt.Errorf("metric %q has no %q plugin config", metric.Name, Name)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, seqtest.Params{}, fmt.Errorf("parsing plugin config: %w", err)
	}
	if c.Address == "" {
		return c, seqtest.Params{}, errors.New("address is required")
	}
	for _, q := range []string{c.Canary.Bad, c.Canary.Total, c.Baseline.Bad, c.Baseline.Total} {
		if strings.TrimSpace(q) == "" {
			return c, seqtest.Params{}, errors.New("canary and baseline need both bad and total queries")
		}
	}
	if c.Alpha == 0 {
		c.Alpha = 0.05
	}
	if c.Alpha <= 0 || c.Alpha >= 1 {
		return c, seqtest.Params{}, errors.New("alpha must be between 0 and 1")
	}
	p := seqtest.DefaultParams
	if c.Margin != nil {
		p.Margin = *c.Margin
	}
	if c.MaxRatio != 0 {
		p.MaxRatio = c.MaxRatio
	}
	if c.Points != 0 {
		p.Points = c.Points
	}
	if _, err := seqtest.New(p); err != nil {
		return c, seqtest.Params{}, err
	}
	return c, p, nil
}

// Querier evaluates one PromQL expression to a single number.
type Querier interface {
	Query(ctx context.Context, query string, at time.Time) (float64, error)
}

// RpcPlugin is the provider served over go-plugin RPC.
type RpcPlugin struct {
	LogCtx     log.Entry
	NewQuerier func(address string) (Querier, error)
	Now        func() time.Time
}

func (p *RpcPlugin) InitPlugin() types.RpcError { return types.RpcError{} }

// lastState returns the newest finished measurement of this metric that
// carries test state. Errored measurements carry none, so a failed query only
// widens the next window instead of losing data.
func lastState(run *v1alpha1.AnalysisRun, metric string) *v1alpha1.Measurement {
	if run == nil {
		return nil
	}
	for _, mr := range run.Status.MetricResults {
		if mr.Name != metric {
			continue
		}
		for i := len(mr.Measurements) - 1; i >= 0; i-- {
			m := mr.Measurements[i]
			if m.Metadata[MetaState] != "" && m.Metadata[MetaWindowEnd] != "" {
				return &mr.Measurements[i]
			}
		}
	}
	return nil
}

func windowStart(run *v1alpha1.AnalysisRun, prev *v1alpha1.Measurement) (time.Time, error) {
	if prev != nil {
		return time.Parse(time.RFC3339Nano, prev.Metadata[MetaWindowEnd])
	}
	if run != nil && run.Status.StartedAt != nil {
		return run.Status.StartedAt.Time, nil
	}
	if run != nil && !run.CreationTimestamp.IsZero() {
		return run.CreationTimestamp.Time, nil
	}
	return time.Time{}, errors.New("cannot tell when the analysis started")
}

// promDuration renders a window for a PromQL range selector, at least 1s.
func promDuration(d time.Duration) string {
	return strconv.FormatInt(max(int64(math.Ceil(d.Seconds())), 1), 10) + "s"
}

func (p *RpcPlugin) Run(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric) v1alpha1.Measurement {
	started := timeutil.MetaNow()
	m := v1alpha1.Measurement{StartedAt: &started}

	cfg, params, err := parseConfig(metric)
	if err != nil {
		return metricutil.MarkMeasurementError(m, err)
	}
	prev := lastState(run, metric.Name)
	var test *seqtest.Test
	if prev != nil {
		test, err = seqtest.Restore(params, prev.Metadata[MetaState])
	} else {
		test, err = seqtest.New(params)
	}
	if err != nil {
		return metricutil.MarkMeasurementError(m, err)
	}
	from, err := windowStart(run, prev)
	if err != nil {
		return metricutil.MarkMeasurementError(m, err)
	}
	now := p.Now()
	window := promDuration(now.Sub(from))

	q, err := p.NewQuerier(cfg.Address)
	if err != nil {
		return metricutil.MarkMeasurementError(m, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var vals [4]float64
	for i, expr := range []string{cfg.Canary.Bad, cfg.Canary.Total, cfg.Baseline.Bad, cfg.Baseline.Total} {
		v, err := q.Query(ctx, strings.ReplaceAll(expr, WindowPlaceholder, window), now)
		if err != nil {
			return metricutil.MarkMeasurementError(m, fmt.Errorf("query %d: %w", i+1, err))
		}
		vals[i] = v
	}
	// increase() extrapolates each series separately, so bad can exceed total
	// by a fraction of an event; clamp instead of failing the measurement.
	iv := seqtest.Interval{
		CanaryBad: math.Min(vals[0], vals[1]), CanaryTotal: vals[1],
		BaselineBad: math.Min(vals[2], vals[3]), BaselineTotal: vals[3],
	}
	if err := test.Observe(iv); err != nil {
		return metricutil.MarkMeasurementError(m, err)
	}

	m.Value = strconv.FormatFloat(test.EValue(), 'g', 4, 64)
	m.Phase = v1alpha1.AnalysisPhaseSuccessful
	if test.Reject(cfg.Alpha) {
		m.Phase = v1alpha1.AnalysisPhaseFailed
	}
	m.Metadata = map[string]string{
		MetaState:     test.MarshalState(),
		MetaWindowEnd: now.UTC().Format(time.RFC3339Nano),
		MetaEValue:    m.Value,
		MetaThreshold: strconv.FormatFloat(1/cfg.Alpha, 'g', 4, 64),
		MetaWindow:    window,
		MetaCounts: fmt.Sprintf("canary %g/%g bad, baseline %g/%g bad",
			iv.CanaryBad, iv.CanaryTotal, iv.BaselineBad, iv.BaselineTotal),
	}
	finished := metav1.NewTime(time.Now())
	m.FinishedAt = &finished
	return m
}

func (p *RpcPlugin) Resume(_ *v1alpha1.AnalysisRun, _ v1alpha1.Metric, m v1alpha1.Measurement) v1alpha1.Measurement {
	return m
}

func (p *RpcPlugin) Terminate(_ *v1alpha1.AnalysisRun, _ v1alpha1.Metric, m v1alpha1.Measurement) v1alpha1.Measurement {
	return m
}

func (p *RpcPlugin) GarbageCollect(*v1alpha1.AnalysisRun, v1alpha1.Metric, int) types.RpcError {
	return types.RpcError{}
}

func (p *RpcPlugin) Type() string { return plugin.ProviderType }

func (p *RpcPlugin) GetMetadata(metric v1alpha1.Metric) map[string]string {
	cfg, params, err := parseConfig(metric)
	if err != nil {
		return nil
	}
	return map[string]string{
		"canaryBadQuery":     cfg.Canary.Bad,
		"canaryTotalQuery":   cfg.Canary.Total,
		"baselineBadQuery":   cfg.Baseline.Bad,
		"baselineTotalQuery": cfg.Baseline.Total,
		"margin":             strconv.FormatFloat(params.Margin, 'g', -1, 64),
		"alpha":              strconv.FormatFloat(cfg.Alpha, 'g', -1, 64),
	}
}
