package store

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// Latency as a log-bucketed histogram (spec 013 #2). A percentile cannot be
// summed, which is what makes the rollup possible at all: a histogram can, so
// a day is 24 hour-rows merged, a re-roll overwrites one row idempotently, and
// the answer stops depending on whether the raw rows still exist.
//
// Bucket i covers [1.25^i, 1.25^(i+1)) milliseconds for i = 0…73, with one
// underflow bucket below 1 ms — 75 counts in all (spec 013 #10). The ratio
// bounds the relative error at ±12% worst case and about ±6% typical, which
// is inside the noise of a latency chart. The last of those 74 is open: a
// trace's latency is the span of a whole run and nothing at ingest bounds it,
// so everything from 1.25^73 — about 3.3 hours — up lands there and reads
// back as that edge rather than being lost. The bounded error therefore holds
// from 1 ms to 3.3 hours, and above it the answer is a floor rather than an
// estimate (corrected in review of PR #28, where the comment claimed the
// bound reached one bucket further than it does).
const (
	histogramRatio = 1.25
	// histogramBuckets counts the underflow bucket plus the 74 log ones.
	histogramBuckets = 75
)

// histogramEdges[i] is the lower bound, in milliseconds, of the log bucket
// stored at index i+1 — 1.25^i, computed as such. Accumulating the edges by
// repeated multiplication instead drifts by a few parts in 10^10 by the top
// of the range, which is enough to put a value that sits exactly on an edge
// into the bucket below it; one table, read by both the index and the
// read-back, keeps the two halves of the contract identical.
var histogramEdges = func() []float64 {
	edges := make([]float64, histogramBuckets-1)
	for i := range edges {
		edges[i] = math.Pow(histogramRatio, float64(i))
	}
	return edges
}()

// Histogram is the count per bucket. A nil histogram is a valid empty one, so
// a caller can range over rows and add to it without a constructor.
type Histogram []int64

// NewHistogram returns a histogram with every bucket at zero.
func NewHistogram() Histogram { return make(Histogram, histogramBuckets) }

// Add records one latency in milliseconds.
func (h *Histogram) Add(ms int64) {
	if len(*h) < histogramBuckets {
		h.grow()
	}
	(*h)[histogramIndex(ms)]++
}

// Merge adds another histogram's counts into this one. Merging is what makes
// a day out of its hours and a re-rolled hour out of its parts.
func (h *Histogram) Merge(other Histogram) {
	if other.Count() == 0 {
		return
	}
	if len(*h) < histogramBuckets {
		h.grow()
	}
	for i, count := range other {
		(*h)[i] += count
	}
}

func (h *Histogram) grow() {
	grown := make(Histogram, histogramBuckets)
	copy(grown, *h)
	*h = grown
}

// Count is how many latencies the histogram holds.
func (h Histogram) Count() int64 {
	var total int64
	for _, count := range h {
		total += count
	}
	return total
}

// Percentile returns the nearest-rank percentile in milliseconds, and whether
// there was anything to report. The value is the geometric middle of the
// bucket the rank falls in — the point that makes the error symmetric, and so
// the ±12% the docs promise; the open top bucket has no middle and answers
// its lower edge; the underflow bucket answers 0, which is what "under a
// millisecond" rounds to in a column of milliseconds.
func (h Histogram) Percentile(p int) (int64, bool) {
	total := h.Count()
	if total == 0 {
		return 0, false
	}
	rank := int64(math.Ceil(float64(p) / 100 * float64(total)))
	if rank < 1 {
		rank = 1
	}
	var seen int64
	for i, count := range h {
		seen += count
		if seen >= rank {
			return histogramValue(i), true
		}
	}
	return histogramValue(len(h) - 1), true
}

// histogramIndex is the bucket a latency belongs in. The search is over the
// same edges the read-back uses, so a value exactly on an edge lands in the
// bucket that edge opens.
func histogramIndex(ms int64) int {
	if ms < 1 {
		return 0
	}
	value := float64(ms)
	above := sort.Search(len(histogramEdges), func(i int) bool {
		return histogramEdges[i] > value
	})
	if above > histogramBuckets-1 {
		above = histogramBuckets - 1
	}
	return above
}

// histogramValue is what a bucket reads back as.
func histogramValue(index int) int64 {
	switch {
	case index <= 0:
		return 0
	case index >= histogramBuckets-1:
		// The open bucket: its lower edge is the only honest floor.
		return int64(math.Round(histogramEdges[len(histogramEdges)-1]))
	}
	edge := histogramEdges[index-1]
	return int64(math.Round(edge * math.Sqrt(histogramRatio)))
}

// MarshalJSON encodes the counts, dropping the trailing zeros. A histogram is
// stored on every rollup row, and most rows hold latencies of one order of
// magnitude: writing 75 numbers where 8 carry information would make the
// column the largest thing in a table whose whole point is being small.
// UnmarshalJSON pads what it reads, so the two are inverses.
func (h Histogram) MarshalJSON() ([]byte, error) {
	last := -1
	for i, count := range h {
		if count != 0 {
			last = i
		}
	}
	return json.Marshal([]int64(h)[:last+1])
}

// UnmarshalJSON reads the counts and pads them back to full width.
func (h *Histogram) UnmarshalJSON(data []byte) error {
	var counts []int64
	if err := json.Unmarshal(data, &counts); err != nil {
		return err
	}
	if len(counts) > histogramBuckets {
		return fmt.Errorf("histogram has %d buckets, want at most %d",
			len(counts), histogramBuckets)
	}
	*h = make(Histogram, histogramBuckets)
	copy(*h, counts)
	return nil
}

// encodeHistogram renders a histogram for its column.
func encodeHistogram(h Histogram) (string, error) {
	encoded, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("encode histogram: %w", err)
	}
	return string(encoded), nil
}

// decodeHistogram reads a histogram out of its column.
func decodeHistogram(encoded string) (Histogram, error) {
	var h Histogram
	if err := json.Unmarshal([]byte(encoded), &h); err != nil {
		return nil, fmt.Errorf("decode histogram %q: %w", encoded, err)
	}
	return h, nil
}
