// Package seqtest implements an anytime-valid sequential test that a canary's
// bad-event rate is not worse than its baseline's by more than a margin.
//
// Model. In each measurement window the canary serves n requests and the
// baseline m. Bad events (errors, slow requests) are treated as Poisson with
// per-request rates lc and lb. Given k = x + y bad events in the window, the
// canary's share x is Binomial(k, theta) with theta = r*n / (r*n + m), where
// r = lc/lb. Because theta is computed per window from that window's traffic,
// the test stays valid when the canary weight changes between steps.
//
// Test. H0: r <= r0 = 1 + margin. For each alternative ratio r_j > r0 on a
// log-spaced grid, the running product of per-window likelihood ratios
// L(r_j)/L(r0) is a non-negative supermartingale under H0 (the binomial family
// has a monotone likelihood ratio in r). Their average, the e-value, is one
// too, so by Ville's inequality P(e-value ever >= 1/alpha) <= alpha no matter
// how often it is checked.
package seqtest

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Interval holds the events counted in one measurement window.
type Interval struct {
	CanaryBad, CanaryTotal     float64
	BaselineBad, BaselineTotal float64
}

// Validate rejects negative counts and bad counts above totals.
func (iv Interval) Validate() error {
	for _, v := range []float64{iv.CanaryBad, iv.CanaryTotal, iv.BaselineBad, iv.BaselineTotal} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("counts must be finite and non-negative: %+v", iv)
		}
	}
	if iv.CanaryBad > iv.CanaryTotal || iv.BaselineBad > iv.BaselineTotal {
		return fmt.Errorf("bad events exceed total events: %+v", iv)
	}
	return nil
}

// Params configures the test. Margin is the tolerated relative regression
// (0.1 lets the canary be up to 10% worse); MaxRatio is the largest
// regression the alternative grid covers; Points is the grid size.
type Params struct {
	Margin   float64
	MaxRatio float64
	Points   int
}

// DefaultParams are used when a field is zero.
var DefaultParams = Params{Margin: 0.1, MaxRatio: 20, Points: 32}

func (p Params) withDefaults() Params {
	if p.MaxRatio == 0 {
		p.MaxRatio = DefaultParams.MaxRatio
	}
	if p.Points == 0 {
		p.Points = DefaultParams.Points
	}
	return p
}

// Test is the running state of one sequential test.
type Test struct {
	p     Params
	r0    float64
	grid  []float64
	logLR []float64
}

// New returns a test with no observations.
func New(p Params) (*Test, error) {
	p = p.withDefaults()
	if p.Margin < 0 || math.IsNaN(p.Margin) {
		return nil, errors.New("margin must be >= 0")
	}
	r0 := 1 + p.Margin
	if !(p.MaxRatio > r0) {
		return nil, fmt.Errorf("maxRatio must be greater than 1+margin (%g)", r0)
	}
	if p.Points < 1 || p.Points > 256 {
		return nil, errors.New("points must be between 1 and 256")
	}
	grid := make([]float64, p.Points)
	lo, hi := math.Log(r0), math.Log(p.MaxRatio)
	for i := range grid {
		grid[i] = math.Exp(lo + (hi-lo)*float64(i+1)/float64(p.Points))
	}
	return &Test{p: p, r0: r0, grid: grid, logLR: make([]float64, p.Points)}, nil
}

// Observe adds one window. A window with no traffic on either side or no bad
// events carries no information about r and leaves the test unchanged.
func (t *Test) Observe(iv Interval) error {
	if err := iv.Validate(); err != nil {
		return err
	}
	n, m := iv.CanaryTotal, iv.BaselineTotal
	x := iv.CanaryBad
	k := x + iv.BaselineBad
	if n == 0 || m == 0 || k == 0 {
		return nil
	}
	th0 := t.r0 * n / (t.r0*n + m)
	for i, r := range t.grid {
		th := r * n / (r*n + m)
		t.logLR[i] += x*math.Log(th/th0) + (k-x)*math.Log((1-th)/(1-th0))
	}
	return nil
}

// LogE returns the log of the e-value: the mean of the grid's likelihood ratios.
func (t *Test) LogE() float64 {
	maxv := math.Inf(-1)
	for _, v := range t.logLR {
		maxv = math.Max(maxv, v)
	}
	var s float64
	for _, v := range t.logLR {
		s += math.Exp(v - maxv)
	}
	return maxv + math.Log(s/float64(len(t.logLR)))
}

// EValue returns the e-value, capped to avoid overflow in reports.
func (t *Test) EValue() float64 { return math.Exp(math.Min(t.LogE(), 700)) }

// Reject reports whether H0 (canary no worse than baseline beyond the margin)
// is rejected at level alpha.
func (t *Test) Reject(alpha float64) bool {
	return t.LogE() >= math.Log(1/alpha)
}

const stateVersion = "v1"

// MarshalState encodes the running state so it can be stored between
// measurements, together with the parameters that produced it.
func (t *Test) MarshalState() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s;%s;%s;%d;", stateVersion,
		strconv.FormatFloat(t.p.Margin, 'g', -1, 64),
		strconv.FormatFloat(t.p.MaxRatio, 'g', -1, 64), t.p.Points)
	for i, v := range t.logLR {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	}
	return b.String()
}

// Restore rebuilds a test from MarshalState output. It fails if the state was
// produced with different parameters: changing them mid-analysis would mix two
// different tests and void the guarantee.
func Restore(p Params, state string) (*Test, error) {
	t, err := New(p)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(state, ";")
	if len(parts) != 5 || parts[0] != stateVersion {
		return nil, errors.New("unrecognised state")
	}
	if parts[1] != strconv.FormatFloat(t.p.Margin, 'g', -1, 64) ||
		parts[2] != strconv.FormatFloat(t.p.MaxRatio, 'g', -1, 64) ||
		parts[3] != strconv.Itoa(t.p.Points) {
		return nil, errors.New("state was produced with different margin, maxRatio or points")
	}
	vals := strings.Split(parts[4], ",")
	if len(vals) != len(t.logLR) {
		return nil, errors.New("state has the wrong number of grid points")
	}
	for i, s := range vals {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) {
			return nil, fmt.Errorf("bad state value %q", s)
		}
		t.logLR[i] = v
	}
	return t, nil
}
