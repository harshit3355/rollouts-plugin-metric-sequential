package sim

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestBinomialMeanAndVariance(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	for _, c := range []struct {
		n int
		p float64
	}{{5000, 0.001}, {500, 0.05}, {2000, 0.3}, {1000, 0.9}} {
		const reps = 20000
		var s, s2 float64
		for range reps {
			v := float64(binomial(rng, c.n, c.p))
			s += v
			s2 += v * v
		}
		mean := s / reps
		variance := s2/reps - mean*mean
		wantM, wantV := float64(c.n)*c.p, float64(c.n)*c.p*(1-c.p)
		if math.Abs(mean-wantM) > 5*math.Sqrt(wantV/reps) {
			t.Errorf("n=%d p=%g: mean %.3f, want %.3f", c.n, c.p, mean, wantM)
		}
		if math.Abs(variance-wantV)/wantV > 0.05 {
			t.Errorf("n=%d p=%g: variance %.3f, want %.3f", c.n, c.p, variance, wantV)
		}
	}
}

func TestRunIsDeterministic(t *testing.T) {
	s := Scenario{BaseRate: 0.01, Effect: 2, Traffic: 1000}
	a, b := Run(s, 50, 42), Run(s, 50, 42)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed gave different results: %+v vs %+v", a[i], b[i])
		}
	}
}
