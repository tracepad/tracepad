package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/tracepad/tracepad/internal/config"
)

// The response budget (spec 004 #2). The consumer's context window is a scarce
// resource the API has to respect, so a payload that would not fit is cut at a
// UTF-8 boundary and replaced by a marker that says how big it really is and
// where the whole of it lives. Silent truncation and unbounded responses waste
// the consumer's budget in opposite directions; a marker lets it choose.
//
// Only payloads are budgeted. The skeleton of a response — the trace, the
// tree, the row fields — is always rendered whole, because a truncated
// structure is not a smaller answer, it is a wrong one (spec 004, edge cases).

// truncation is the marker that replaces a payload too large to inline. It
// carries the ready-made URL for an HTTP consumer and the raw id pair for an
// MCP one, whose `get_observation_io` tool takes exactly that pair (#2, #17).
type truncation struct {
	Truncated     bool   `json:"truncated"`
	Size          int    `json:"size"`
	Preview       string `json:"preview,omitempty"`
	TraceID       string `json:"trace_id"`
	ObservationID string `json:"observation_id"`
	Full          string `json:"full"`
}

// ioPath is where the whole payload lives: the one budget-exempt endpoint
// (#3), which is why a marker can always name a working follow-up.
func ioPath(traceID, observationID string) string {
	return fmt.Sprintf("/api/v1/observations/%s/io?trace_id=%s",
		url.PathEscape(observationID), url.QueryEscape(traceID))
}

// ioKeyOverhead is what a payload key costs before its value: `,"metadata":`
// is the longest of the three (#6), and reserving the worst case keeps the
// arithmetic below an upper bound rather than an estimate.
const ioKeyOverhead = len(`,"metadata":`)

// minPreview is the shortest prefix worth showing. Below it a preview says
// nothing about the payload's shape and the marker alone is more honest.
const minPreview = 32

// previewKeyOverhead is what `,"preview":` costs before the string itself.
// markerSize cannot see it — Preview is omitempty and empty when measured —
// so a preview sized against the raw share overshoots by exactly this much.
const previewKeyOverhead = len(`,"preview":`)

// payloadBudget divides what is left of a response budget among the payloads
// that want to be inlined. Every payload gets an equal share (#6): a debugging
// agent usually needs the shape of every IO and the full text of one or two,
// and an equal share plus a marker naming the follow-up serves exactly that.
type payloadBudget struct {
	share int
	// affordable reports whether the share covers even a bare marker. When
	// it does not, nothing is inlined at all: markers are not free, and a
	// trace wide enough to make them unaffordable would otherwise blow the
	// budget by a multiple of it (Decision 30).
	affordable bool
}

// newPayloadBudget splits `total - skeleton` between `slots` payloads, having
// first reserved what the payload keys themselves will cost.
//
// A trace with no payloads at all is affordable, not refused: there is nothing
// to spend the budget on, and answering "the budget cannot carry markers for 0
// payloads" would turn every trace from a plain exporter into a complaint
// (found in review of PR #5).
func newPayloadBudget(total, skeleton, slots int) payloadBudget {
	if slots <= 0 {
		return payloadBudget{affordable: true}
	}
	remaining := total - skeleton - slots*ioKeyOverhead
	if remaining < 0 {
		return payloadBudget{}
	}
	share := remaining / slots
	return payloadBudget{share: share, affordable: share >= bareMarkerSize}
}

// bareMarkerSize is what one marker costs with no preview: the two ids, the
// size, the flag and the URL. Computed once from the real shape rather than
// guessed, so it stays true if the marker gains a field.
var bareMarkerSize = markerSize(truncation{
	Truncated:     true,
	Size:          1 << 40,
	TraceID:       strings.Repeat("f", 32),
	ObservationID: strings.Repeat("f", 16),
	Full:          ioPath(strings.Repeat("f", 32), strings.Repeat("f", 16)),
})

// budgetNeeded is what `?budget=` would have to be for this response to carry
// a marker for every payload. Handing the number back is the difference
// between "no payloads for you" and a request the caller can actually retry.
//
// It is never clamped up to the maximum: a number the caller would be refused
// for asking is worse than a big one, because "retry with this" that fails
// identically is a loop an automated consumer cannot leave (found in review of
// PR #5). When the true need is above the ceiling, `retryable` is false and the
// reason says so instead.
func budgetNeeded(skeleton, slots int) (needed int, retryable bool) {
	needed = skeleton + slots*(ioKeyOverhead+bareMarkerSize)
	if needed < config.MinResponseBudgetBytes {
		needed = config.MinResponseBudgetBytes
	}
	return needed, needed <= config.MaxResponseBudgetBytes
}

// render returns the value to inline for one payload: the value itself when it
// fits, a marker when it does not.
func (b payloadBudget) render(value any, traceID, observationID string) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		// A payload was decoded from stored JSON, so it re-encodes; if
		// that ever stopped being true, the marker is still a true
		// statement about a payload the consumer cannot have inline.
		return truncation{
			Truncated: true, TraceID: traceID, ObservationID: observationID,
			Full: ioPath(traceID, observationID),
		}
	}
	if len(encoded) <= b.share {
		return value
	}

	marker := truncation{
		Truncated:     true,
		Size:          len(encoded),
		TraceID:       traceID,
		ObservationID: observationID,
		Full:          ioPath(traceID, observationID),
	}
	room := b.share - markerSize(marker) - previewKeyOverhead
	if room >= minPreview {
		marker.Preview = fitString(string(encoded), room)
	}
	return marker
}

// markerSize is what a marker costs on the wire without its preview.
func markerSize(marker truncation) int {
	encoded, err := json.Marshal(marker)
	if err != nil {
		return 0
	}
	return len(encoded)
}

// fitString cuts text so that its JSON encoding fits in room bytes, on a
// rune boundary. The loop exists because escaping is not a fixed cost: a
// payload full of quotes doubles, one of plain prose does not, and guessing
// either way would waste budget or overrun it.
func fitString(text string, room int) string {
	cut := min(room, len(text))
	for range 8 {
		candidate := text[:runeBoundary(text, cut)]
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return ""
		}
		if len(encoded) <= room {
			return candidate
		}
		// Shrink in proportion to the overrun; two quotes of the
		// encoding are the delimiters, which the cut cannot remove.
		next := cut * (room - 2) / (len(encoded) - 2)
		if next >= cut {
			next = cut - 1
		}
		if next <= 0 {
			return ""
		}
		cut = next
	}
	return ""
}

// runeBoundary moves a byte offset back to the start of the rune it lands in,
// so a cut payload is still valid UTF-8 (#2).
func runeBoundary(text string, offset int) int {
	if offset >= len(text) {
		return len(text)
	}
	for offset > 0 && !utf8.RuneStart(text[offset]) {
		offset--
	}
	return offset
}
