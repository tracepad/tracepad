package store

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"sort"
	"testing"
)

// The histogram is what makes a percentile summable (spec 013 #2): the tests
// below are the three properties the rollup leans on — the buckets are where
// the spec says, a merge equals the concatenation, and an empty histogram
// reports nothing rather than zero.

func TestHistogramBucketEdges(t *testing.T) {
	for _, tc := range []struct {
		ms    int64
		index int
		why   string
	}{
		{0, 0, "under a millisecond is the underflow bucket"},
		{-5, 0, "a latency below zero cannot happen, and is not a reason to panic"},
		{1, 1, "the first log bucket opens exactly at 1 ms"},
		{2, 4, "1.25^3 = 1.95, so 2 ms opens the fourth"},
		{1_274_473, 63, "just under 1.25^63 — the edge the spec's first draft stopped at"},
		{1_274_474, 64, "and just over it, in the bucket that edge opens"},
		{14_836_825, histogramBuckets - 1, "1.25^74 ≈ 4.1 h opens the open bucket"},
		{math.MaxInt32, histogramBuckets - 1, "and everything above stays in it"},
	} {
		if got := histogramIndex(tc.ms); got != tc.index {
			t.Errorf("histogramIndex(%d) = %d, want %d — %s", tc.ms, got, tc.index, tc.why)
		}
	}

	// Every edge lands in the bucket it opens, which is the property that
	// makes the read-back and the index agree.
	for i, edge := range histogramEdges {
		if got := histogramIndex(int64(math.Ceil(edge))); got < i+1 {
			t.Errorf("edge %d (%.2f ms) fell into bucket %d, want %d or above", i, edge, got, i+1)
		}
	}
}

// The bound the docs promise: a percentile read back out of the histogram is
// within the bucket ratio of the exact one. Checked over a spread that covers
// four orders of magnitude, which is what a real latency chart holds.
func TestHistogramPercentileStaysWithinTheBound(t *testing.T) {
	random := rand.New(rand.NewPCG(13, 27))
	samples := make([]int64, 0, 5000)
	h := NewHistogram()
	for range 5000 {
		// Log-uniform between 1 ms and 100 s, the shape of a latency
		// distribution rather than a uniform one.
		ms := int64(math.Exp(random.Float64() * math.Log(100_000)))
		samples = append(samples, ms)
		h.Add(ms)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	for _, p := range []int{50, 95, 99} {
		exact := samples[int(math.Ceil(float64(p)/100*float64(len(samples))))-1]
		got, ok := h.Percentile(p)
		if !ok {
			t.Fatalf("p%d reported nothing over %d samples", p, len(samples))
		}
		// Half a bucket either way is the whole error budget: the value
		// is the bucket's geometric middle and the truth is somewhere in
		// the bucket.
		if ratio := float64(got) / float64(exact); ratio < 1/math.Sqrt(histogramRatio)*0.999 ||
			ratio > math.Sqrt(histogramRatio)*1.001 {
			t.Errorf("p%d = %d, exact = %d (ratio %.3f), want within ±12%%",
				p, got, exact, ratio)
		}
	}
}

// Merging is the operation the whole rollup rests on: a day is its hours, a
// re-rolled hour is its parts, and either must equal the histogram of all the
// samples at once.
func TestHistogramMergeEqualsTheConcatenation(t *testing.T) {
	first, second, both := NewHistogram(), NewHistogram(), NewHistogram()
	for ms := int64(1); ms < 4000; ms += 7 {
		first.Add(ms)
		both.Add(ms)
	}
	for ms := int64(3); ms < 9000; ms += 11 {
		second.Add(ms)
		both.Add(ms)
	}

	merged := NewHistogram()
	merged.Merge(first)
	merged.Merge(second)

	for i := range both {
		if merged[i] != both[i] {
			t.Fatalf("bucket %d: merged %d, concatenated %d", i, merged[i], both[i])
		}
	}
	if merged.Count() != both.Count() {
		t.Errorf("count = %d, want %d", merged.Count(), both.Count())
	}
}

// A bucket nothing was measured in reports nothing. Zero would be a lie of
// the same kind the cost column refuses to tell (spec 002 #14).
func TestHistogramEmptyReportsNothing(t *testing.T) {
	for _, h := range []Histogram{nil, NewHistogram()} {
		if value, ok := h.Percentile(50); ok {
			t.Errorf("p50 = %d over an empty histogram, want nothing", value)
		}
	}
}

// A nil histogram takes an Add without a constructor, which is what lets the
// aggregator range over rows and build one per dimension tuple.
func TestHistogramGrowsFromNil(t *testing.T) {
	var h Histogram
	h.Add(42)
	if h.Count() != 1 {
		t.Fatalf("count = %d, want 1", h.Count())
	}
	if value, ok := h.Percentile(50); !ok || value < 37 || value > 47 {
		t.Errorf("p50 = %d (%v), want 42 within the bucket", value, ok)
	}
}

func TestHistogramRoundTripsThroughItsColumn(t *testing.T) {
	h := NewHistogram()
	for _, ms := range []int64{0, 1, 40, 900, 25_000} {
		h.Add(ms)
	}

	encoded, err := encodeHistogram(h)
	if err != nil {
		t.Fatal(err)
	}
	// The trailing zeros are dropped: the column is on every row of the
	// table this spec exists to keep small.
	var counts []int64
	if err := json.Unmarshal([]byte(encoded), &counts); err != nil {
		t.Fatal(err)
	}
	if len(counts) >= histogramBuckets {
		t.Errorf("encoded %d counts, want the trailing zeros dropped", len(counts))
	}

	back, err := decodeHistogram(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != histogramBuckets {
		t.Fatalf("decoded width %d, want %d", len(back), histogramBuckets)
	}
	for i := range h {
		if back[i] != h[i] {
			t.Fatalf("bucket %d: %d after the round trip, %d before", i, back[i], h[i])
		}
	}
}

func TestHistogramRefusesAnOversizedColumn(t *testing.T) {
	if _, err := decodeHistogram("[" + repeatCounts(histogramBuckets+1) + "]"); err == nil {
		t.Error("a histogram wider than the layout was accepted")
	}
}

func repeatCounts(n int) string {
	out := ""
	for i := range n {
		if i > 0 {
			out += ","
		}
		out += "1"
	}
	return out
}
