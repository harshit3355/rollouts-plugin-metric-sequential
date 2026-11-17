package seqtest

import (
	"math"
	"math/rand/v2"
	"testing"
)

// rollout simulates one canary under the test's own model: each window has k
// bad events, and each is the canary's with probability theta(r) for that
// window's traffic split. It returns true if the test ever rejects.
func rollout(t *testing.T, rng *rand.Rand, ratio float64) bool {
	t.Helper()
	tst, err := New(Params{Margin: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []float64{0.05, 0.1, 0.25, 0.5} {
		for range 10 {
			n, m := 1000*w, 1000*(1-w)
			k := 5 + rng.IntN(20)
			th := ratio * n / (ratio*n + m)
			x := 0
			for range k {
				if rng.Float64() < th {
					x++
				}
			}
			if err := tst.Observe(Interval{float64(x), n, float64(k - x), m}); err != nil {
				t.Fatal(err)
			}
			if tst.Reject(0.05) {
				return true
			}
		}
	}
	return false
}

func TestFalseRollbackRateAtNullBoundary(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	const reps = 4000
	rejected := 0
	for range reps {
		if rollout(t, rng, 1.1) {
			rejected++
		}
	}
	rate := float64(rejected) / reps
	// 40 looks with changing weights; the bound is alpha = 0.05 regardless.
	if limit := 0.05 + 3*math.Sqrt(0.05*0.95/reps); rate > limit {
		t.Fatalf("false rollback rate %.4f exceeds %.4f", rate, limit)
	}
	t.Logf("false rollback rate at r = r0: %.4f", rate)
}

func TestDetectsLargeRegression(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	const reps = 500
	detected := 0
	for range reps {
		if rollout(t, rng, 3) {
			detected++
		}
	}
	if rate := float64(detected) / reps; rate < 0.95 {
		t.Fatalf("detected only %.3f of 3x regressions", rate)
	}
}

func TestWindowsWithoutInformationAreIgnored(t *testing.T) {
	tst, _ := New(Params{})
	before := tst.MarshalState()
	for _, iv := range []Interval{
		{0, 0, 3, 100},   // no canary traffic
		{2, 100, 0, 0},   // no baseline traffic
		{0, 100, 0, 900}, // no bad events
	} {
		if err := tst.Observe(iv); err != nil {
			t.Fatal(err)
		}
	}
	if tst.MarshalState() != before || tst.LogE() != 0 {
		t.Fatalf("state changed without information: %s", tst.MarshalState())
	}
}

func TestStateRoundTripAndParameterGuard(t *testing.T) {
	p := Params{Margin: 0.2, MaxRatio: 10, Points: 8}
	a, _ := New(p)
	_ = a.Observe(Interval{12, 500, 20, 4500})
	b, err := Restore(p, a.MarshalState())
	if err != nil {
		t.Fatal(err)
	}
	if a.LogE() != b.LogE() {
		t.Fatalf("round trip changed e-value: %v vs %v", a.LogE(), b.LogE())
	}
	if _, err := Restore(Params{Margin: 0.1, MaxRatio: 10, Points: 8}, a.MarshalState()); err == nil {
		t.Fatal("restoring with a different margin must fail")
	}
	if _, err := Restore(p, "garbage"); err == nil {
		t.Fatal("garbage state must fail")
	}
}

func TestValidate(t *testing.T) {
	bad := []Interval{
		{-1, 10, 0, 10},
		{11, 10, 0, 10},
		{0, 10, math.NaN(), 10},
	}
	for _, iv := range bad {
		if iv.Validate() == nil {
			t.Fatalf("expected error for %+v", iv)
		}
	}
	if _, err := New(Params{Margin: 0.1, MaxRatio: 1.05}); err == nil {
		t.Fatal("maxRatio below 1+margin must fail")
	}
}
