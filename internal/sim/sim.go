// Package sim replays simulated canary rollouts through several rollback rules
// so their false-rollback rate, detection rate and delay can be compared on
// identical traffic.
package sim

import (
	"math"
	"math/rand/v2"

	"github.com/harshit3355/rollouts-plugin-metric-sequential/internal/seqtest"
)

// Weights and StepWindows describe the rollout: canary weight per step and the
// number of analysis windows (looks) spent at each step.
var (
	Weights     = []float64{0.05, 0.10, 0.25, 0.50}
	StepWindows = 10
)

// Scenario is one traffic and regression setting.
type Scenario struct {
	BaseRate float64 `json:"baseRate"`           // baseline bad-event rate per request
	Effect   float64 `json:"effect"`             // canary rate / baseline rate
	Traffic  int     `json:"trafficPerInterval"` // requests per analysis window
}

// Window is one analysis window's counts.
type Window struct {
	CanaryBad, CanaryTotal, BaselineBad, BaselineTotal float64
}

// Method decides, after each window, whether to roll back. It returns the
// 0-based index of the window at which it rolls back, or -1.
type Method struct {
	Name   string
	Decide func([]Window) int
}

// Alpha and Margin are shared by every method that has them.
const (
	Alpha  = 0.05
	Margin = 0.1
)

// binomial draws an exact Binomial(n, p) sample by skipping over geometric
// gaps between successes, which takes about n*min(p,1-p) steps.
func binomial(rng *rand.Rand, n int, p float64) int {
	if p <= 0 || n == 0 {
		return 0
	}
	if p >= 1 {
		return n
	}
	if p > 0.5 {
		return n - binomial(rng, n, 1-p)
	}
	logq := math.Log1p(-p)
	count, pos := 0, 0
	for {
		pos += int(math.Floor(math.Log(1-rng.Float64())/logq)) + 1
		if pos > n {
			return count
		}
		count++
	}
}

// Stream simulates one rollout's windows.
func Stream(rng *rand.Rand, s Scenario) []Window {
	out := make([]Window, 0, len(Weights)*StepWindows)
	pc := math.Min(s.BaseRate*s.Effect, 1)
	for _, w := range Weights {
		for range StepWindows {
			n := binomial(rng, s.Traffic, w)
			m := s.Traffic - n
			out = append(out, Window{
				CanaryBad:     float64(binomial(rng, n, pc)),
				CanaryTotal:   float64(n),
				BaselineBad:   float64(binomial(rng, m, s.BaseRate)),
				BaselineTotal: float64(m),
			})
		}
	}
	return out
}

func sequential(margin float64) func([]Window) int {
	return func(ws []Window) int {
		t, _ := seqtest.New(seqtest.Params{Margin: margin})
		for i, w := range ws {
			_ = t.Observe(seqtest.Interval(w))
			if t.Reject(Alpha) {
				return i
			}
		}
		return -1
	}
}

// pooled is the ablation without per-window state: at every look it runs the
// same test once on the cumulative counts, as a stateless query would.
func pooled(ws []Window) int {
	var c Window
	for i, w := range ws {
		c.CanaryBad += w.CanaryBad
		c.CanaryTotal += w.CanaryTotal
		c.BaselineBad += w.BaselineBad
		c.BaselineTotal += w.BaselineTotal
		t, _ := seqtest.New(seqtest.Params{Margin: Margin})
		_ = t.Observe(seqtest.Interval(c))
		if t.Reject(Alpha) {
			return i
		}
	}
	return -1
}

// zScore is the one-sided two-proportion statistic for H0: pc <= (1+Margin)*pb.
func zScore(c Window) float64 {
	if c.CanaryTotal == 0 || c.BaselineTotal == 0 {
		return 0
	}
	pc, pb := c.CanaryBad/c.CanaryTotal, c.BaselineBad/c.BaselineTotal
	r0 := 1 + Margin
	se := math.Sqrt(pc*(1-pc)/c.CanaryTotal + r0*r0*pb*(1-pb)/c.BaselineTotal)
	if se == 0 {
		return 0
	}
	return (pc - r0*pb) / se
}

const zCrit = 1.6448536269514722 // one-sided 5%

// peekingZ applies a fixed-sample z-test at every look: the common mistake.
func peekingZ(ws []Window) int {
	var c Window
	for i, w := range ws {
		c.CanaryBad += w.CanaryBad
		c.CanaryTotal += w.CanaryTotal
		c.BaselineBad += w.BaselineBad
		c.BaselineTotal += w.BaselineTotal
		if zScore(c) > zCrit {
			return i
		}
	}
	return -1
}

// fixedHorizonZ applies the z-test once, after the last window.
func fixedHorizonZ(ws []Window) int {
	var c Window
	for _, w := range ws {
		c.CanaryBad += w.CanaryBad
		c.CanaryTotal += w.CanaryTotal
		c.BaselineBad += w.BaselineBad
		c.BaselineTotal += w.BaselineTotal
	}
	if zScore(c) > zCrit {
		return len(ws) - 1
	}
	return -1
}

// threshold mimics a typical AnalysisTemplate: a window fails when the
// canary's bad rate exceeds factor x the known baseline rate, and the analysis
// fails once more than failureLimit windows have failed.
func threshold(baseRate, factor float64, failureLimit int) func([]Window) int {
	return func(ws []Window) int {
		failed := 0
		for i, w := range ws {
			if w.CanaryTotal > 0 && w.CanaryBad/w.CanaryTotal > factor*baseRate {
				failed++
				if failed > failureLimit {
					return i
				}
			}
		}
		return -1
	}
}

// Methods returns the compared rules. The threshold rule needs the baseline
// rate, which an operator would take from history.
func Methods(baseRate float64) []Method {
	return []Method{
		{"sequential", sequential(Margin)},
		{"threshold-2x-failureLimit-2", threshold(baseRate, 2, 2)},
		{"peeking-z", peekingZ},
		{"fixed-horizon-z", fixedHorizonZ},
		{"ablation-pooled", pooled},
		{"ablation-no-margin", sequential(0)},
	}
}

// Result summarises one method on one scenario.
type Result struct {
	Scenario          Scenario `json:"scenario"`
	Method            string   `json:"method"`
	Reps              int      `json:"reps"`
	RollbackRate      float64  `json:"rollbackRate"`
	MeanDetectionLook float64  `json:"meanDetectionLook"` // 1-based, over rolled-back reps; 0 if none
	MeanExcessBad     float64  `json:"meanExcessBadEvents"`
}

// Run simulates reps rollouts for s and scores every method on the same streams.
func Run(s Scenario, reps int, seed uint64) []Result {
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	ms := Methods(s.BaseRate)
	rolled := make([]int, len(ms))
	lookSum := make([]float64, len(ms))
	excess := make([]float64, len(ms))
	pc := math.Min(s.BaseRate*s.Effect, 1)
	for range reps {
		ws := Stream(rng, s)
		for j, m := range ms {
			at := m.Decide(ws)
			last := len(ws) - 1
			if at >= 0 {
				rolled[j]++
				lookSum[j] += float64(at + 1)
				last = at
			}
			// Expected extra bad events served by the canary before the decision.
			for _, w := range ws[:last+1] {
				excess[j] += math.Max(pc-s.BaseRate, 0) * w.CanaryTotal
			}
		}
	}
	out := make([]Result, len(ms))
	for j, m := range ms {
		r := Result{Scenario: s, Method: m.Name, Reps: reps,
			RollbackRate:  float64(rolled[j]) / float64(reps),
			MeanExcessBad: excess[j] / float64(reps)}
		if rolled[j] > 0 {
			r.MeanDetectionLook = lookSum[j] / float64(rolled[j])
		}
		out[j] = r
	}
	return out
}
