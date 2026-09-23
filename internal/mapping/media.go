package mapping

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Media (spec 041): images and files taken out of the JSON at ingest, stored
// once, and left behind as a reference.
//
// The walk runs over the decoded export *before* the mapper sees it, and
// rewrites the attributes in place (Decision 12 of the spec). That one pass is
// what both halves of #5 need: the mapper then maps a body whose payloads
// already carry references, and the raw batch kept for replay is that same
// body re-encoded — so a replay of the archive maps to exactly the rows the
// first delivery produced, with no second walker that could disagree with the
// first about what was media.
//
// Four shapes are recognised (#1), and nothing else is: a data URL string,
// Anthropic's base64 source, Gemini's inline data, and the GenAI conventions'
// blob part. A base64 string with no MIME type beside it is not media — it is
// a string, and guessing what bytes are is how a trace's text gets mangled.

// MediaMinSize is the decoded size from which a match is extracted (#1).
// Below it the reference would cost more than it saves.
const MediaMinSize = 4096

// minEncodedMedia is the shortest base64 that can decode to MediaMinSize
// bytes. A string shorter than this cannot hold extractable media unless it
// names a Langfuse upload, which is what lets the walk skip nearly every
// attribute of an ordinary export without looking inside it.
const minEncodedMedia = MediaMinSize / 3 * 4

// The reference object (#4), and the keys it is made of.
const (
	MediaRefKey    = "tracepad_media"
	mediaMimeKey   = "mime_type"
	mediaSizeKey   = "size"
	mediaStoredKey = "stored"
)

// langfuseMarker opens the reference string the Langfuse SDK leaves where it
// took a picture out (#9): `@@@langfuseMedia:type=…|id=…|source=…@@@`.
const langfuseMarker = "@@@langfuseMedia:"

// MediaOptions is what one project's ingest needs to know.
type MediaOptions struct {
	// Placeholder is the project setting of #6: the reference is left with
	// `"stored": false` and no body is kept.
	Placeholder bool
	// Resolve answers a Langfuse media id with the body it names, when
	// this project holds a ref to one (#9). Nil resolves nothing.
	Resolve func(mediaID string) (sha string, size int64, ok bool)
	// Warn is told about a malformed body under a recognised shape, which
	// is left inline (spec 041, edge cases). Nil says nothing.
	Warn func(traceID, reason string)
}

// MediaBody is one distinct body the walk extracted.
type MediaBody struct {
	SHA256   string
	MimeType string
	Body     []byte
}

// MediaRef is one trace pointing at one body (#3).
type MediaRef struct {
	SHA256  string
	TraceID string
}

// MediaResult is what one walk found.
type MediaResult struct {
	// Bodies are the distinct bodies to store, in the order first seen.
	// Empty under the placeholder setting.
	Bodies []*MediaBody
	// Refs are the distinct (body, trace) pairs: every extracted body and
	// every resolved Langfuse upload, for the traces whose spans carried it.
	Refs []MediaRef
	// Changed reports, per ResourceSpans, whether the walk rewrote it —
	// what decides whether the raw body has to be re-encoded at all.
	Changed []bool
}

// Any reports whether the walk rewrote anything.
func (r *MediaResult) Any() bool { return anyTrue(r.Changed) }

// SHAs lists every body the export now points at, once each: what the raw
// batch's own refs are (Decision 12).
func (r *MediaResult) SHAs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(sha string) {
		if !seen[sha] {
			seen[sha] = true
			out = append(out, sha)
		}
	}
	for _, body := range r.Bodies {
		add(body.SHA256)
	}
	for _, ref := range r.Refs {
		add(ref.SHA256)
	}
	return out
}

// ExtractMedia walks every attribute of an export — resource, scope, span and
// event — and replaces each recognised media value with a reference, in place.
func ExtractMedia(resourceSpans []*tracepb.ResourceSpans, opts MediaOptions) *MediaResult {
	w := &mediaWalk{
		opts:   opts,
		res:    &MediaResult{Changed: make([]bool, len(resourceSpans))},
		bodies: map[string]bool{},
		refs:   map[MediaRef]bool{},
	}
	for i, rs := range resourceSpans {
		if rs == nil {
			continue
		}
		changed := false
		all := tracesOf(rs.GetScopeSpans()...)
		if resource := rs.GetResource(); resource != nil {
			w.traces = all
			changed = w.attributes(resource.Attributes) || changed
		}
		for _, ss := range rs.GetScopeSpans() {
			if ss == nil {
				continue
			}
			if scope := ss.GetScope(); scope != nil {
				w.traces = tracesOf(ss)
				changed = w.attributes(scope.Attributes) || changed
			}
			for _, span := range ss.GetSpans() {
				if span == nil {
					continue
				}
				w.traces = nil
				if id := traceID(span.GetTraceId()); id != "" {
					w.traces = []string{id}
				}
				changed = w.attributes(span.Attributes) || changed
				for _, event := range span.GetEvents() {
					if event != nil {
						changed = w.attributes(event.Attributes) || changed
					}
				}
			}
		}
		w.res.Changed[i] = changed
	}
	return w.res
}

// tracesOf lists the distinct trace ids of the spans under some scopes: the
// traces a resource or scope attribute reaches.
func tracesOf(scopes ...*tracepb.ScopeSpans) []string {
	seen := map[string]bool{}
	var out []string
	for _, ss := range scopes {
		for _, span := range ss.GetSpans() {
			if id := traceID(span.GetTraceId()); id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

type mediaWalk struct {
	opts   MediaOptions
	res    *MediaResult
	bodies map[string]bool
	refs   map[MediaRef]bool
	// traces are the trace ids the value being walked belongs to.
	traces []string
}

func (w *mediaWalk) attributes(kvs []*commonpb.KeyValue) bool {
	changed := false
	for _, kv := range kvs {
		if kv != nil && w.anyValue(kv.Value) {
			changed = true
		}
	}
	return changed
}

// anyValue rewrites one OTLP value in place and reports whether it did.
func (w *mediaWalk) anyValue(v *commonpb.AnyValue) bool {
	if v == nil {
		return false
	}
	switch value := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		replacement, changed := w.text(value.StringValue)
		if !changed {
			return false
		}
		switch r := replacement.(type) {
		case string:
			value.StringValue = r
		case map[string]any:
			v.Value = &commonpb.AnyValue_KvlistValue{KvlistValue: refKvlist(r)}
		}
		return true
	case *commonpb.AnyValue_ArrayValue:
		changed := false
		for _, item := range value.ArrayValue.GetValues() {
			if w.anyValue(item) {
				changed = true
			}
		}
		return changed
	case *commonpb.AnyValue_KvlistValue:
		kvs := value.KvlistValue.GetValues()
		if ref, matched := w.shape(kvlistGetter(kvs)); matched {
			if ref == nil {
				return false
			}
			v.Value = &commonpb.AnyValue_KvlistValue{KvlistValue: refKvlist(ref)}
			return true
		}
		return w.attributes(kvs)
	}
	return false
}

// text is one string: the whole of it a data URL or a Langfuse reference, or
// a JSON document that may carry either inside. The answer is a reference
// object, the re-encoded document, or no change.
func (w *mediaWalk) text(s string) (any, bool) {
	if ref := w.wholeString(s); ref != nil {
		return ref, true
	}
	if len(s) < minEncodedMedia && !strings.Contains(s, langfuseMarker) {
		return nil, false
	}
	if !mayHoldMedia(s) {
		return nil, false
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}
	document, ok := decodeDocument(trimmed)
	if !ok {
		return nil, false
	}
	rewritten, changed := w.jsonValue(document)
	if !changed {
		return nil, false
	}
	encoded, ok := encodeDocument(rewritten)
	if !ok {
		return nil, false
	}
	return encoded, true
}

// wholeString matches a string that is, in its entirety, a data URL or a
// Langfuse reference (#1: a data URL inside a longer string is not a match).
func (w *mediaWalk) wholeString(s string) map[string]any {
	if mime, data, ok := parseDataURL(s); ok {
		return w.reference(mime, data)
	}
	if mime, id, ok := parseLangfuseRef(s); ok {
		return w.langfuse(mime, id)
	}
	return nil
}

// jsonValue walks a decoded document, answering its replacement.
func (w *mediaWalk) jsonValue(v any) (any, bool) {
	switch value := v.(type) {
	case string:
		if ref := w.wholeString(value); ref != nil {
			return ref, true
		}
	case []any:
		changed := false
		for i, item := range value {
			if replacement, ok := w.jsonValue(item); ok {
				value[i] = replacement
				changed = true
			}
		}
		return value, changed
	case map[string]any:
		if ref, matched := w.shape(mapGetter(value)); matched {
			if ref == nil {
				return value, false
			}
			return ref, true
		}
		changed := false
		for key, item := range value {
			if replacement, ok := w.jsonValue(item); ok {
				value[key] = replacement
				changed = true
			}
		}
		return value, changed
	}
	return v, false
}

// getter reads a string field of an object, and nested an object field, over
// either spelling of an object: a decoded JSON map or an OTLP kvlist.
type getter struct {
	text   func(key string) (string, bool)
	nested func(key string) (getter, bool)
}

func mapGetter(m map[string]any) getter {
	return getter{
		text: func(key string) (string, bool) {
			s, ok := m[key].(string)
			return s, ok
		},
		nested: func(key string) (getter, bool) {
			inner, ok := m[key].(map[string]any)
			if !ok {
				return getter{}, false
			}
			return mapGetter(inner), true
		},
	}
}

func kvlistGetter(kvs []*commonpb.KeyValue) getter {
	find := func(key string) *commonpb.AnyValue {
		for _, kv := range kvs {
			if kv != nil && kv.Key == key {
				return kv.Value
			}
		}
		return nil
	}
	return getter{
		text: func(key string) (string, bool) {
			v, ok := find(key).GetValue().(*commonpb.AnyValue_StringValue)
			if !ok {
				return "", false
			}
			return v.StringValue, true
		},
		nested: func(key string) (getter, bool) {
			v, ok := find(key).GetValue().(*commonpb.AnyValue_KvlistValue)
			if !ok {
				return getter{}, false
			}
			return kvlistGetter(v.KvlistValue.GetValues()), true
		},
	}
}

// shape recognises the three object shapes of #1 and answers the reference
// that replaces the whole object. `matched` without a reference is an object
// of a media shape that stays as it is — too small, malformed, or an
// unresolved Langfuse id — and whose insides need no further walk.
func (w *mediaWalk) shape(g getter) (ref map[string]any, matched bool) {
	mime, data, ok := objectMedia(g)
	if !ok {
		return nil, false
	}
	if _, id, isRef := parseLangfuseRef(data); isRef {
		return w.langfuse(mime, id), true
	}
	return w.reference(mime, data), true
}

// objectMedia is the three object shapes: Anthropic's base64 source, the
// GenAI conventions' blob part, and Gemini's inline data under either
// spelling.
func objectMedia(g getter) (mime, data string, ok bool) {
	kind, _ := g.text("type")
	switch kind {
	case "base64":
		mime, okMime := g.text("media_type")
		data, okData := g.text("data")
		if okMime && okData {
			return mime, data, true
		}
	case "blob":
		mime, okMime := g.text("mime_type")
		data, okData := g.text("content")
		if okMime && okData {
			return mime, data, true
		}
	}
	for _, key := range []string{"inline_data", "inlineData"} {
		inner, found := g.nested(key)
		if !found {
			continue
		}
		mime, okMime := inner.text("mime_type")
		if !okMime {
			mime, okMime = inner.text("mimeType")
		}
		data, okData := inner.text("data")
		if okMime && okData {
			return mime, data, true
		}
	}
	return "", "", false
}

// reference decodes one match and answers its reference, or nil to leave it
// inline: below the size floor, or not base64 at all.
func (w *mediaWalk) reference(mime, data string) map[string]any {
	if mime == "" || len(data) < minEncodedMedia {
		return nil
	}
	body, ok := decodeBase64(data)
	if !ok {
		w.warn("a " + mime + " body under a recognised media shape is not base64; it was left inline")
		return nil
	}
	if len(body) < MediaMinSize {
		return nil
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	ref := map[string]any{MediaRefKey: sha, mediaMimeKey: mime, mediaSizeKey: int64(len(body))}
	if w.opts.Placeholder {
		ref[mediaStoredKey] = false
		return ref
	}
	if !w.bodies[sha] {
		w.bodies[sha] = true
		w.res.Bodies = append(w.res.Bodies, &MediaBody{SHA256: sha, MimeType: mime, Body: body})
	}
	w.addRefs(sha)
	return ref
}

// langfuse resolves a Langfuse upload to the body this project holds, or nil
// to leave the reference string as the client wrote it (#9).
func (w *mediaWalk) langfuse(mime, id string) map[string]any {
	if w.opts.Resolve == nil {
		return nil
	}
	sha, size, ok := w.opts.Resolve(id)
	if !ok {
		return nil
	}
	w.addRefs(sha)
	return map[string]any{MediaRefKey: sha, mediaMimeKey: mime, mediaSizeKey: size}
}

func (w *mediaWalk) addRefs(sha string) {
	for _, trace := range w.traces {
		ref := MediaRef{SHA256: sha, TraceID: trace}
		if !w.refs[ref] {
			w.refs[ref] = true
			w.res.Refs = append(w.res.Refs, ref)
		}
	}
}

func (w *mediaWalk) warn(reason string) {
	if w.opts.Warn == nil {
		return
	}
	trace := ""
	if len(w.traces) > 0 {
		trace = w.traces[0]
	}
	w.opts.Warn(trace, reason)
}

// parseDataURL splits `data:<mime>[;params];base64,<data>`. The MIME type is
// required: without one there is nothing to say what the bytes are.
func parseDataURL(s string) (mime, data string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	header, data, found := strings.Cut(s[len("data:"):], ",")
	if !found || !strings.HasSuffix(header, ";base64") {
		return "", "", false
	}
	mime, _, _ = strings.Cut(header, ";")
	if !strings.Contains(mime, "/") {
		return "", "", false
	}
	return mime, data, true
}

// parseLangfuseRef reads the reference string the Langfuse SDK writes
// (`langfuse/media.py`, `_reference_string`), whole.
func parseLangfuseRef(s string) (mime, id string, ok bool) {
	if !strings.HasPrefix(s, langfuseMarker) || !strings.HasSuffix(s, "@@@") ||
		len(s) < len(langfuseMarker)+3 {
		return "", "", false
	}
	for _, part := range strings.Split(s[len(langfuseMarker):len(s)-3], "|") {
		key, value, _ := strings.Cut(part, "=")
		switch key {
		case "type":
			mime = value
		case "id":
			id = value
		}
	}
	return mime, id, id != ""
}

// decodeBase64 takes the standard alphabet, padded or not, and the URL-safe
// one the Langfuse SDK also accepts.
func decodeBase64(data string) ([]byte, bool) {
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if body, err := encoding.DecodeString(data); err == nil {
			return body, true
		}
	}
	return nil, false
}

// mayHoldMedia is the cheap look before the expensive one: a document with
// none of these in it has nothing the walk could match, and decoding a large
// payload twice for nothing is what the check exists to avoid.
func mayHoldMedia(s string) bool {
	for _, needle := range []string{";base64,", `"base64"`, `"blob"`, "inline_data", "inlineData", langfuseMarker} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// decodeDocument reads one JSON value, numbers kept as written so that a
// re-encoding does not round them.
func decodeDocument(s string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil || decoder.More() {
		return nil, false
	}
	return v, true
}

// encodeDocument writes a document back without the HTML escaping
// `json.Marshal` applies, which would turn every `<` of a prompt into `<`.
func encodeDocument(v any) (string, bool) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// refKvlist renders a reference object as the OTLP value that replaces a
// string attribute which was, whole, a data URL. The mapper reads a kvlist as
// an object, so the payload holds the same reference either way.
func refKvlist(ref map[string]any) *commonpb.KeyValueList {
	list := &commonpb.KeyValueList{}
	add := func(key string, value *commonpb.AnyValue) {
		list.Values = append(list.Values, &commonpb.KeyValue{Key: key, Value: value})
	}
	add(MediaRefKey, stringValue(ref[MediaRefKey]))
	add(mediaMimeKey, stringValue(ref[mediaMimeKey]))
	if size, ok := ref[mediaSizeKey].(int64); ok {
		add(mediaSizeKey, &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: size}})
	}
	if stored, ok := ref[mediaStoredKey].(bool); ok {
		add(mediaStoredKey, &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: stored}})
	}
	return list
}

func stringValue(v any) *commonpb.AnyValue {
	s, _ := v.(string)
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}
