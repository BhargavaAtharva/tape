package hist

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestBucketsAreContiguous(t *testing.T) {
	for i := 0; i < size-1; i++ {
		u := upper(i)
		if index(u) != i {
			t.Fatalf("upper(%d)=%d lands in bucket %d", i, u, index(u))
		}
		if index(u+1) != i+1 {
			t.Fatalf("value %d after bucket %d lands in bucket %d", u+1, i, index(u+1))
		}
	}
	if index(^uint64(0)) != size-1 {
		t.Fatalf("max value lands in bucket %d of %d", index(^uint64(0)), size)
	}
}

func TestQuantileWithinErrorBound(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	var h Histogram
	samples := make([]int64, 100_000)
	for i := range samples {
		samples[i] = int64(rng.ExpFloat64() * 50_000)
		h.Record(samples[i])
	}
	slices.Sort(samples)
	c := h.Snapshot()
	for _, q := range []float64{0.5, 0.9, 0.99, 0.999} {
		exact := samples[int(q*float64(len(samples)))-1]
		got := c.Quantile(q)
		if got < exact || float64(got) > float64(exact)*1.07+1 {
			t.Errorf("q%.3f: got %d, exact %d", q, got, exact)
		}
	}
}

func TestSubGivesTheWindow(t *testing.T) {
	var h Histogram
	h.Record(10)
	before := h.Snapshot()
	h.Record(1000)
	h.Record(1000)
	w := h.Snapshot().Sub(before)
	if w.Total() != 2 {
		t.Fatalf("window holds %d samples", w.Total())
	}
	if q := w.Quantile(0.5); q < 1000 || q > 1070 {
		t.Fatalf("window median %d", q)
	}
	var empty Counts
	if empty.Quantile(0.99) != 0 {
		t.Fatal("empty histogram should report 0")
	}
}
