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
	spans := mediaSpans(text)
	// The room the media fields take is settled once, from what the payload
	// holds, and the preview is cut to what is left (spec 004 #39): no search
	// for a fixed point, and a payload with no media pays nothing.
	var distinct []mediaSpan
	seen := map[string]bool{}
	for _, one := range spans {
		if !seen[one.key] {
			seen[one.key] = true
			distinct = append(distinct, one)
		}
	}
	marker.MediaCount = len(distinct)
	avail := b.share - markerSize(marker) - previewKeyOverhead
	if len(distinct) == 0 || avail < 0 {
		// Nothing to name, or a share that cannot carry even the count.
		marker.MediaCount = 0
		if room := b.share - markerSize(marker) - previewKeyOverhead; room >= minPreview {
			marker.Preview = fitString(text, room)
		}
		return marker
	}

	// The list's room: the longest entry's size for each of them, up to the limit, and never so
	// much that the preview has less than minPreview left (unless the share has
	// no room for a preview at all, and then the list may have it).
	var longest int
	for _, one := range distinct {
		longest = max(longest, len(one.encoded)+1)
	}
	reserve := longest*min(len(distinct), maxListedMedia) + mediaKeyOverhead
	if spare := avail - minPreview; spare >= mediaKeyOverhead {
		reserve = min(reserve, spare)
	} else {
		reserve = min(reserve, max(avail, 0))
	}
	if reserve > 0 && reserve <= mediaKeyOverhead {
		reserve = 0 // not room for one entry: the count alone
	}
	if room := avail - reserve; room >= minPreview {
		marker.Preview = fitString(text, room)
	}

	// Past the preview: the references whose object it does not hold whole,
	// in document order — the first of them first. A body shown anywhere in
	// the preview is shown.
	shown := map[string]bool{}
	for _, one := range spans {
		if one.end <= len(marker.Preview) {
			shown[one.key] = true
		}
	}
	var beyond []mediaSpan
	for _, one := range distinct {
		if !shown[one.key] {
			beyond = append(beyond, one)
		}
	}
	marker.MediaCount = len(beyond)
	if reserve > 0 {
		room := reserve - mediaKeyOverhead
		for _, one := range beyond {
			if len(marker.Media) == maxListedMedia || len(one.encoded)+1 > room {
				break
			}
			room -= len(one.encoded) + 1
			marker.Media = append(marker.Media, one.ref)
		}
	}
	return marker
}

// mediaSpan is one media reference in a payload's JSON text: where its object
// lies, the reference as stored, and the body it names (a placeholder, which
// holds no bytes, is another from the same SHA-256).
type mediaSpan struct {
	start, end int
	key        string
	ref        map[string]any
	encoded    []byte
}

// mediaSpans finds the media references (spec 041 #4) in the JSON text of a
// payload, in document order, by structure: the text is walked once, strings
// skipped whole, and an object that has a `tracepad_media` member holding a
// SHA-256 and a string `mime_type` is a reference from its `{` to its `}`,
// whatever else it carries and in whatever order. A hash that appears inside a
// string is text, however it is quoted.
func mediaSpans(text string) []mediaSpan {
	const key = mapping.MediaRefKey
	var (
		spans     []mediaSpan
		open      []int            // where each container still open began
		holders   = map[int]bool{} // the objects that hold a hash under the key
		lastKey   string
		afterKey  bool
		isKeyNext = func(from int) bool {
			for ; from < len(text); from++ {
				switch text[from] {
				case ' ', '\t', '\n', '\r':
				default:
					return text[from] == ':'
				}
			}
			return false
		}
	)
	for i := 0; i < len(text); i++ {
		switch c := text[i]; c {
		case '"':
			end := i + 1
			for end < len(text) && text[end] != '"' {
				if text[end] == '\\' {
					end++
				}
				end++
			}
			content := ""
			if end <= len(text) && end > i+1 {
				content = text[i+1 : min(end, len(text))]
			}
			switch {
			case isKeyNext(end + 1):
				lastKey, afterKey = content, true
			case afterKey && lastKey == key && isSHA256Hex(content) && len(open) > 0 && text[open[len(open)-1]] == '{':
				holders[open[len(open)-1]] = true
				afterKey = false
			default:
				afterKey = false
			}
			i = end
		case '{', '[':
			open = append(open, i)
			afterKey = false
		case '}', ']':
			if len(open) == 0 {
				continue
			}
			start := open[len(open)-1]
			open = open[:len(open)-1]
			afterKey = false
			if !holders[start] {
				continue
			}
			delete(holders, start)
			var ref map[string]any
			decoder := json.NewDecoder(strings.NewReader(text[start : i+1]))
			decoder.UseNumber()
			if decoder.Decode(&ref) != nil {
				continue
			}
			sha, _ := ref[key].(string)
			if _, typed := ref["mime_type"].(string); !typed || !isSHA256Hex(sha) {
				continue
			}
			one := mediaSpan{start: start, end: i + 1, key: sha, ref: ref, encoded: []byte(text[start : i+1])}
			if stored, ok := ref["stored"].(bool); ok && !stored {
				one.key += ":placeholder"
			}
			spans = append(spans, one)
		}
	}
	slices.SortFunc(spans, func(a, b mediaSpan) int { return a.start - b.start })
	return spans
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
