package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mapping"
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
//
// A payload that holds media (spec 041 #4) says so when the cut leaves a
// reference out: `media_count` is how many references lie beyond the preview,
// `media` the first of them, each exactly as it is stored. Without that, an
// image the application sent looked lost when its reference sat after the cut
// (spec 004 #39).
type truncation struct {
	Truncated     bool             `json:"truncated"`
	Size          int              `json:"size"`
	Preview       string           `json:"preview,omitempty"`
	TraceID       string           `json:"trace_id"`
	ObservationID string           `json:"observation_id"`
	Full          string           `json:"full"`
	MediaCount    int              `json:"media_count,omitempty"`
	Media         []map[string]any `json:"media,omitempty"`
}

// maxListedMedia bounds the references a marker spells out; `media_count` says
// how many there are. The marker lives inside a budget of its own share.
const maxListedMedia = 16

// mediaKeyOverhead is what the `media` array costs before its first entry.
const mediaKeyOverhead = len(`,"media":[]`)

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
	// A count is worth its few bytes in every marker: whether the cut hid
	// media is the one thing a consumer cannot find out from a preview.
	MediaCount: 1 << 20,
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
		// The bytes it was measured by are the bytes it is written as:
		// encoded once, not again when the answer is (spec 043 #28).
		return encodedPayload(encoded)
	}

	marker := truncation{
		Truncated:     true,
		Size:          len(encoded),
		TraceID:       traceID,
		ObservationID: observationID,
		Full:          ioPath(traceID, observationID),
	}
	text := string(encoded)
	refs := mediaReferences(value)
	if len(refs) == 0 {
		if room := b.share - markerSize(marker) - previewKeyOverhead; room >= minPreview {
			marker.Preview = fitString(text, room)
		}
		return marker
	}

	// The references the preview does not reach are what the marker is least
	// able to do without, and each one listed shortens the preview, which can
	// leave one more reference beyond it. So the two are settled together: the
	// preview is cut, what lies past it is listed, the preview is cut again
	// with the list's room taken, and so on until the list stops growing — it
	// only grows, and is bounded, so it does. What the preview holds whole is
	// never listed.
	ends := mediaEnds(text)
	beyond := func(preview string) []map[string]any {
		var out []map[string]any
		for _, ref := range refs {
			if end, ok := ends[ref[mapping.MediaRefKey].(string)]; !ok || end > len(preview) {
				out = append(out, ref)
			}
		}
		return out
	}
	cut := func(listed []map[string]any) string {
		marker.MediaCount, marker.Media = len(refs), listed
		if room := b.share - markerSize(marker) - previewKeyOverhead; room >= minPreview {
			return fitString(text, room)
		}
		return ""
	}
	var listed []map[string]any
	preview := cut(nil)
	for range maxListedMedia + 1 {
		next := fitReferences(beyond(preview), b.share-bareMarkerSize-mediaKeyOverhead)
		if len(next) == len(listed) {
			break
		}
		listed = next
		preview = cut(listed)
	}
	marker.Preview = preview
	marker.Media = listed
	marker.MediaCount = len(beyond(preview))
	if marker.MediaCount == 0 {
		marker.Media = nil
	}
	return marker
}

// mediaEnds is where each media reference ends in the payload's JSON text, by
// body: the offset just past the reference's object, the first time the body
// appears. The text is what json.Marshal wrote, in which a reference is
// `{"mime_type":…,"size":…,"tracepad_media":"<sha>"}` — `tracepad_media` is the
// last key a reference has, and a quote inside a string is escaped, so the
// sequence below is only ever a key and never text a prompt quoted.
func mediaEnds(text string) map[string]int {
	const key = `"` + mapping.MediaRefKey + `":"`
	ends := map[string]int{}
	for from := 0; ; {
		at := strings.Index(text[from:], key)
		if at < 0 {
			return ends
		}
		start := from + at + len(key)
		from = start
		if start+64 > len(text) || !isSHA256Hex(text[start:start+64]) {
			continue
		}
		if _, seen := ends[text[start:start+64]]; !seen {
			ends[text[start:start+64]] = start + 64 + len(`"}`)
		}
	}
}

// mediaReferences are the media references in a payload (spec 041 #4), once
// each, in document order: an object whose `tracepad_media` is a SHA-256 in
// hex and that carries a `mime_type`, which is what the interface recognises
// as one too. A body sent twice is one reference; a placeholder, which holds no
// bytes, is another from the same body.
func mediaReferences(value any) []map[string]any {
	var found []map[string]any
	seen := map[string]bool{}
	var walk func(node any)
	walk = func(node any) {
		switch node := node.(type) {
		case map[string]any:
			if sha, ok := node[mapping.MediaRefKey].(string); ok && isSHA256Hex(sha) {
				if _, ok := node["mime_type"].(string); ok {
					key := sha
					if stored, ok := node["stored"].(bool); ok && !stored {
						key += ":placeholder"
					}
					if !seen[key] {
						seen[key] = true
						found = append(found, node)
					}
					return
				}
			}
			keys := make([]string, 0, len(node))
			for key := range node {
				keys = append(keys, key)
			}
			slices.Sort(keys) // the order json.Marshal writes them in
			for _, key := range keys {
				walk(node[key])
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(value)
	return found
}

// isSHA256Hex says whether text is a lowercase hex SHA-256, 64 characters.
func isSHA256Hex(text string) bool {
	if len(text) != 64 {
		return false
	}
	for i := 0; i < len(text); i++ {
		if c := text[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// fitReferences takes as many references, first to last, as fit in room bytes
// and in maxListedMedia, commas included.
func fitReferences(refs []map[string]any, room int) []map[string]any {
	var fit []map[string]any
	for _, ref := range refs {
		if len(fit) == maxListedMedia {
			break
		}
		encoded, err := json.Marshal(ref)
		if err != nil || len(encoded)+1 > room {
			break
		}
		room -= len(encoded) + 1
		fit = append(fit, ref)
	}
	return fit
}

// encodedPayload is a payload encoding/json has already written, placed in an
// answer as it is. The bytes are json.Marshal's, escaping included, so they
// are what encoding the value again would write.
type encodedPayload []byte

func (p encodedPayload) appendJSON(buffer *bytes.Buffer) error {
	buffer.Write(p)
	return nil
}

func (p encodedPayload) MarshalJSON() ([]byte, error) { return p, nil }

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
