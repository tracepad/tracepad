package mapping

import (
	"encoding/base64"
	"strings"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// MediaFetch answers a body by its hash: its stored MIME type and bytes, or
// false when there is none to give.
type MediaFetch func(sha string) (mime string, body []byte, ok bool)

// InlineMedia is ExtractMedia run backwards, for the way out (spec 041 #8): a
// reference becomes media again, so a batch leaving for another backend is
// whole. It reports, per ResourceSpans, whether it rewrote anything.
//
// Each shape comes back as it was sent (Decision 19): a reference in the slot
// of an object shape becomes the base64 that slot held, and any other
// reference — which replaced a whole string — becomes a data URL. Posted back
// to a Tracepad, either is extracted to the same reference. A reference with
// `"stored": false`, or whose body is gone, stays a reference.
func InlineMedia(resourceSpans []*tracepb.ResourceSpans, fetch MediaFetch) []bool {
	return rewriteMedia(resourceSpans, &mediaInline{fetch: fetch, bodies: map[string]inlineBody{}})
}

type mediaInline struct {
	fetch MediaFetch
	// bodies caches a body's base64: the photograph sent to five
	// generations is fetched and encoded once per batch.
	bodies map[string]inlineBody
}

type inlineBody struct {
	mime, encoded string
	ok            bool
}

func (in *mediaInline) enter([]string) {}

// whole: a reference is always an object, never a string.
func (in *mediaInline) whole(string) (map[string]any, bool) { return nil, false }

func (in *mediaInline) decodes(s string) bool { return strings.Contains(s, MediaRefKey) }

func (in *mediaInline) object(f fields) (string, bool, bool) {
	if holder, key, _, ok := mediaSlot(f); ok {
		if slot, isObject := holder.nested(key); isObject {
			if sha, _, stored, isRef := refOf(slot); isRef {
				body, ok := in.body(sha, stored)
				if !ok {
					return "", false, true
				}
				holder.set(key, body.encoded)
				return "", true, true
			}
		}
	}
	sha, mime, stored, isRef := refOf(f)
	if !isRef {
		return "", false, false
	}
	body, ok := in.body(sha, stored)
	if !ok {
		return "", false, true
	}
	// The type the reference carries wins over the stored one: it is what
	// this client declared (spec 041, edge cases).
	if mime == "" {
		mime = body.mime
	}
	return "data:" + mime + ";base64," + body.encoded, true, true
}

func (in *mediaInline) body(sha string, stored bool) (inlineBody, bool) {
	if !stored || in.fetch == nil {
		return inlineBody{}, false
	}
	body, asked := in.bodies[sha]
	if !asked {
		mime, bytes, ok := in.fetch(sha)
		body = inlineBody{mime: mime, ok: ok}
		if ok {
			body.encoded = base64.StdEncoding.EncodeToString(bytes)
		}
		in.bodies[sha] = body
	}
	return body, body.ok
}
