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
	"google.golang.org/protobuf/encoding/protojson"
)

// Media (spec 041): images and files taken out of the JSON at ingest, stored
// once, and left behind as a reference.
//
// The walk runs over the decoded export *before* the mapper sees it, and
// rewrites the attributes in place (spec 041, Decision 16). That one pass is
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

// LangfuseMarker opens the reference string the Langfuse SDK leaves where it
// took a picture out (#9): `@@@langfuseMedia:type=…|id=…|source=…@@@`.
const LangfuseMarker = "@@@langfuseMedia:"

const langfuseMarker = LangfuseMarker

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
	// Resolved are the bodies a Langfuse reference string was rewritten
	// to: read outside the write, so the write checks they are still there.
	Resolved []string
	// Rewrites are, per ResourceSpans, the values the walk replaced — what
	// decides whether the raw body is rewritten at all, and where.
	Rewrites Rewrites
}

// Any reports whether the walk rewrote anything.
func (r *MediaResult) Any() bool { return r.Rewrites.Any() }

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

// Rewrites is what a walk replaced in each ResourceSpans of an export: the
// values, and where each sits in the element's JSON, so that a JSON body is
// spliced rather than re-encoded (spec 041 #5).
type Rewrites [][]jsonEdit

// Changed reports, per ResourceSpans, whether the walk rewrote it.
func (r Rewrites) Changed() []bool {
	out := make([]bool, len(r))
	for i, edits := range r {
		out[i] = len(edits) > 0
	}
	return out
}

// Any reports whether the walk rewrote anything.
func (r Rewrites) Any() bool { return anyTrue(r.Changed()) }

// ExtractMedia walks every attribute of an export — resource, scope, span and
// event — and replaces each recognised media value with a reference, in place.
func ExtractMedia(resourceSpans []*tracepb.ResourceSpans, opts MediaOptions) *MediaResult {
	w := &mediaWalk{
		opts:     opts,
		res:      &MediaResult{},
		bodies:   map[string]bool{},
		refs:     map[MediaRef]bool{},
		resolved: map[string]resolvedMedia{},
	}
	w.res.Rewrites = rewriteMedia(resourceSpans, w)
	return w.res
}

// mediaRewrite is one direction of the walk over an export: ExtractMedia
// turns media into references, InlineMedia turns them back. Both run on one
// traversal, rewriteMedia, so the two cannot disagree about where a value may
// sit.
type mediaRewrite interface {
	// enter is told which traces the attributes walked next belong to.
	enter(traces []string)
	// whole answers the reference that replaces a string which is, in its
	// entirety, media.
	whole(s string) (map[string]any, bool)
	// object is tried on every object before its fields are walked. It may
	// rewrite the object in place through its fields, answer a string that
	// replaces the object whole, and stop the walk entering it.
	object(f fields) (replacement string, stop bool)
	// decodes is the cheap look at a string before it is decoded as a JSON
	// document to walk.
	decodes(s string) bool
}

// The OTLP/JSON names of the containers a walk goes through, with the proto
// spelling the decoder also accepts.
var (
	stepScopeSpans = memberOr("scopeSpans", "scope_spans")
	stepAttributes = member("attributes")
	stepValue      = member("value")
	stepValues     = member("values")
	stepArray      = memberOr("arrayValue", "array_value")
	stepKvlist     = memberOr("kvlistValue", "kvlist_value")
)

// rewriteMedia runs a rewrite over every attribute of an export — resource,
// scope, span and event — and answers, per ResourceSpans, what it replaced.
func rewriteMedia(resourceSpans []*tracepb.ResourceSpans, r mediaRewrite) Rewrites {
	rewrites := make(Rewrites, len(resourceSpans))
	for i, rs := range resourceSpans {
		if rs == nil {
			continue
		}
		sink := &editSink{}
		visit := func(traces []string, kvs []*commonpb.KeyValue, path ...jsonStep) {
			r.enter(traces)
			rewriteAttributes(r, kvs, within(path, stepAttributes), sink)
		}
		if resource := rs.GetResource(); resource != nil {
			visit(tracesOf(rs.GetScopeSpans()...), resource.Attributes, member("resource"))
		}
		for s, ss := range rs.GetScopeSpans() {
			if ss == nil {
				continue
			}
			if scope := ss.GetScope(); scope != nil {
				visit(tracesOf(ss), scope.Attributes, stepScopeSpans, element(s), member("scope"))
			}
			for k, span := range ss.GetSpans() {
				if span == nil {
					continue
				}
				var traces []string
				if id := traceID(span.GetTraceId()); id != "" {
					traces = []string{id}
				}
				at := []jsonStep{stepScopeSpans, element(s), member("spans"), element(k)}
				visit(traces, span.Attributes, at...)
				for e, event := range span.GetEvents() {
					if event != nil {
						visit(traces, event.Attributes, within(at, member("events"), element(e))...)
					}
				}
			}
		}
		rewrites[i] = sink.edits
	}
	return rewrites
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

// rewriteAttributes walks a list of key-values whose JSON array sits at path.
func rewriteAttributes(r mediaRewrite, kvs []*commonpb.KeyValue, path []jsonStep, sink *editSink) {
	for i, kv := range kvs {
		if kv != nil {
			rewriteAnyValue(r, kv.Value, within(path, element(i), stepValue), sink)
		}
	}
}

// anyValueJSON is an edit's value for an OTLP value: its protojson, compacted
// so that the spliced bytes are stable.
func anyValueJSON(v *commonpb.AnyValue) func() ([]byte, error) {
	return func() ([]byte, error) {
		encoded, err := protojson.Marshal(v)
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		if err := json.Compact(&out, encoded); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}
}

// rewriteAnyValue rewrites one OTLP value, whose JSON sits at path, in place.
func rewriteAnyValue(r mediaRewrite, v *commonpb.AnyValue, path []jsonStep, sink *editSink) {
	if v == nil {
		return
	}
	switch value := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		if ref, ok := r.whole(value.StringValue); ok {
			v.Value = otlpValue(ref).Value
			sink.add(path, anyValueJSON(v))
			return
		}
		if !r.decodes(value.StringValue) {
			return
		}
		if rewritten, ok := rewriteDocument(r, value.StringValue); ok {
			value.StringValue = rewritten
			sink.add(path, anyValueJSON(v))
		}
	case *commonpb.AnyValue_ArrayValue:
		for i, item := range value.ArrayValue.GetValues() {
			rewriteAnyValue(r, item, within(path, stepArray, stepValues, element(i)), sink)
		}
	case *commonpb.AnyValue_KvlistValue:
		kvs := value.KvlistValue.GetValues()
		list := within(path, stepKvlist)
		replacement, stop := r.object(kvlistFields(kvs, list, sink))
		if replacement != "" {
			v.Value = &commonpb.AnyValue_StringValue{StringValue: replacement}
			sink.add(path, anyValueJSON(v))
			return
		}
		if !stop {
			rewriteAttributes(r, kvs, within(list, stepValues), sink)
		}
	}
}

// rewriteDocument is a string attribute that holds a JSON document: decoded,
// walked, and — only when something in it was replaced — spliced, so that
// everything but the replaced values stays as the client wrote it. A
// document that was, whole, one reference goes back to the string it
// replaced rather than to that string's JSON encoding.
func rewriteDocument(r mediaRewrite, s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return "", false
	}
	document, ok := decodeDocument(trimmed)
	if !ok {
		return "", false
	}
	sink := &editSink{}
	replacement, replaced := rewriteJSON(r, document, nil, sink)
	if text, isText := replacement.(string); replaced && isText {
		return text, true
	}
	if len(sink.edits) == 0 {
		return "", false
	}
	lead := strings.Index(s, trimmed)
	spliced, err := spliceJSON([]byte(trimmed), sink.edits)
	if err != nil {
		// Unreachable for a document the decoder read; the rewritten tree
		// is still a true document, only not the client's spelling of it.
		return encodeDocument(replacement)
	}
	return s[:lead] + string(spliced) + s[lead+len(trimmed):], true
}

// rewriteJSON walks a decoded document whose value sits at path. It answers
// the value that replaces this one whole, when there is one; a value
// rewritten inside is changed in place, and the sink told where.
func rewriteJSON(r mediaRewrite, v any, path []jsonStep, sink *editSink) (any, bool) {
	switch value := v.(type) {
	case string:
		if ref, ok := r.whole(value); ok {
			return ref, true
		}
	case []any:
		for i, item := range value {
			at := within(path, element(i))
			if replacement, ok := rewriteJSON(r, item, at, sink); ok {
				value[i] = replacement
				sink.add(at, encodedJSON(replacement))
			}
		}
	case map[string]any:
		replacement, stop := r.object(mapFields(value, path, sink))
		if replacement != "" {
			return replacement, true
		}
		if stop {
			return v, false
		}
		for key, item := range value {
			at := within(path, member(key))
			if replacement, ok := rewriteJSON(r, item, at, sink); ok {
				value[key] = replacement
				sink.add(at, encodedJSON(replacement))
			}
		}
	}
	return v, false
}

// fields reads and writes the fields of an object over either spelling of
// one: a decoded JSON map or an OTLP kvlist. A value set is a string or a
// reference object, and the sink is told where it went.
type fields struct {
	has    func(key string) bool
	text   func(key string) (string, bool)
	flag   func(key string) (bool, bool)
	nested func(key string) (fields, bool)
	set    func(key string, value any)
}

// mapFields is a decoded object whose value sits at path in its document.
func mapFields(m map[string]any, path []jsonStep, sink *editSink) fields {
	return fields{
		has: func(key string) bool {
			_, ok := m[key]
			return ok
		},
		text: func(key string) (string, bool) {
			s, ok := m[key].(string)
			return s, ok
		},
		flag: func(key string) (bool, bool) {
			b, ok := m[key].(bool)
			return b, ok
		},
		nested: func(key string) (fields, bool) {
			inner, ok := m[key].(map[string]any)
			if !ok {
				return fields{}, false
			}
			return mapFields(inner, within(path, member(key)), sink), true
		},
		set: func(key string, value any) {
			m[key] = value
			sink.add(within(path, member(key)), encodedJSON(value))
		},
	}
}

// kvlistFields is a kvlist whose KeyValueList sits at path in the export.
func kvlistFields(kvs []*commonpb.KeyValue, path []jsonStep, sink *editSink) fields {
	find := func(key string) (int, *commonpb.KeyValue) {
		for i, kv := range kvs {
			if kv != nil && kv.Key == key {
				return i, kv
			}
		}
		return -1, nil
	}
	valueOf := func(key string) *commonpb.AnyValue {
		_, kv := find(key)
		return kv.GetValue()
	}
	return fields{
		has: func(key string) bool {
			_, kv := find(key)
			return kv != nil
		},
		text: func(key string) (string, bool) {
			v, ok := valueOf(key).GetValue().(*commonpb.AnyValue_StringValue)
			if !ok {
				return "", false
			}
			return v.StringValue, true
		},
		flag: func(key string) (bool, bool) {
			v, ok := valueOf(key).GetValue().(*commonpb.AnyValue_BoolValue)
			if !ok {
				return false, false
			}
			return v.BoolValue, true
		},
		nested: func(key string) (fields, bool) {
			i, kv := find(key)
			v, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_KvlistValue)
			if !ok {
				return fields{}, false
			}
			return kvlistFields(v.KvlistValue.GetValues(),
				within(path, stepValues, element(i), stepValue, stepKvlist), sink), true
		},
		set: func(key string, value any) {
			if i, kv := find(key); kv != nil {
				kv.Value = otlpValue(value)
				sink.add(within(path, stepValues, element(i), stepValue), anyValueJSON(kv.Value))
			}
		},
	}
}

// mediaSlot finds where one of the three object shapes of #1 keeps its
// bytes: the object holding them, their key, and the declared type.
// Anthropic's base64 source and the GenAI blob part hold them themselves;
// Gemini's part holds them in its inline data, under either spelling.
//
// The reference is written into that slot and every other field of the
// object is left alone (Decision 19): a blob part's `modality` or a Gemini
// part's `thought_signature` is the client's, not the media.
func mediaSlot(f fields) (holder fields, key, mime string, ok bool) {
	kind, _ := f.text("type")
	switch kind {
	case "base64":
		if mime, okMime := f.text("media_type"); okMime && f.has("data") {
			return f, "data", mime, true
		}
	case "blob":
		if mime, okMime := f.text("mime_type"); okMime && f.has("content") {
			return f, "content", mime, true
		}
	}
	for _, name := range []string{"inline_data", "inlineData"} {
		inner, found := f.nested(name)
		if !found {
			continue
		}
		mime, okMime := inner.text("mime_type")
		if !okMime {
			mime, okMime = inner.text("mimeType")
		}
		if okMime && inner.has("data") {
			return inner, "data", mime, true
		}
	}
	return fields{}, "", "", false
}

// refOf reads a reference object.
func refOf(f fields) (sha, mime string, stored, ok bool) {
	sha, ok = f.text(MediaRefKey)
	if !ok {
		return "", "", false, false
	}
	mime, _ = f.text(mediaMimeKey)
	flag, isFlag := f.flag(mediaStoredKey)
	return sha, mime, !isFlag || flag, true
}

type mediaWalk struct {
	opts   MediaOptions
	res    *MediaResult
	bodies map[string]bool
	refs   map[MediaRef]bool
	// resolved memoises the Langfuse ids already asked of the store: a
	// conversation that sends one picture in every turn is one lookup.
	resolved map[string]resolvedMedia
	// traces are the trace ids the value being walked belongs to.
	traces []string
}

type resolvedMedia struct {
	sha  string
	size int64
	ok   bool
}

func (w *mediaWalk) enter(traces []string) { w.traces = traces }

func (w *mediaWalk) whole(s string) (map[string]any, bool) {
	ref := w.wholeString(s)
	return ref, ref != nil
}

func (w *mediaWalk) decodes(s string) bool {
	if len(s) < minEncodedMedia && !strings.Contains(s, langfuseMarker) {
		return false
	}
	return mayHoldMedia(s)
}

// object writes a reference into the slot of an object shape. An object of
// a media shape whose bytes stay — too small, malformed, or an unresolved
// Langfuse id — needs no further walk either.
func (w *mediaWalk) object(f fields) (string, bool) {
	holder, key, mime, ok := mediaSlot(f)
	if !ok {
		return "", false
	}
	data, isText := holder.text(key)
	if !isText {
		return "", false
	}
	var ref map[string]any
	if _, id, isRef := parseLangfuseRef(data); isRef {
		ref = w.langfuse(mime, id)
	} else {
		ref = w.reference(mime, data)
	}
	if ref != nil {
		holder.set(key, ref)
	}
	return "", true
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
	// Under the placeholder setting nothing is kept (#6): resolving would
	// give an upload stored before the switch a new trace to live for.
	if w.opts.Resolve == nil || w.opts.Placeholder {
		return nil
	}
	found, asked := w.resolved[id]
	if !asked {
		found.sha, found.size, found.ok = w.opts.Resolve(id)
		w.resolved[id] = found
		if found.ok {
			w.res.Resolved = append(w.res.Resolved, found.sha)
		}
	}
	if !found.ok {
		return nil
	}
	w.addRefs(found.sha)
	return map[string]any{MediaRefKey: found.sha, mediaMimeKey: mime, mediaSizeKey: found.size}
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

// ResolveLangfuseMedia rewrites, in a payload read back from the store, each
// Langfuse reference string whose upload the trace now points at (spec 041,
// Decision 21). A span can overtake its own upload: it is stored with the
// SDK's string, and the ref arrives with the PUT after it. The stored payload
// keeps the string; a read answers the reference in its place, the same one
// ingest would have written. Nothing else is touched — a data URL a payload
// still holds names no body the store has.
func ResolveLangfuseMedia(v any, resolve func(mediaID string) (sha string, size int64, ok bool)) (any, bool) {
	sink := &editSink{}
	replacement, replaced := rewriteJSON(langfuseRead(resolve), v, nil, sink)
	return replacement, replaced || len(sink.edits) > 0
}

type langfuseRead func(mediaID string) (sha string, size int64, ok bool)

func (r langfuseRead) enter([]string) {}

func (r langfuseRead) decodes(string) bool { return false }

func (r langfuseRead) whole(s string) (map[string]any, bool) {
	mime, id, ok := parseLangfuseRef(s)
	if !ok {
		return nil, false
	}
	return r.reference(mime, id)
}

func (r langfuseRead) object(f fields) (string, bool) {
	holder, key, mime, ok := mediaSlot(f)
	if !ok {
		return "", false
	}
	data, _ := holder.text(key)
	_, id, isRef := parseLangfuseRef(data)
	if !isRef {
		return "", false
	}
	if ref, ok := r.reference(mime, id); ok {
		holder.set(key, ref)
	}
	return "", true
}

func (r langfuseRead) reference(mime, id string) (map[string]any, bool) {
	sha, size, ok := r(id)
	if !ok {
		return nil, false
	}
	return map[string]any{MediaRefKey: sha, mediaMimeKey: mime, mediaSizeKey: size}, true
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

// otlpValue renders what a rewrite sets — a string, or a reference object —
// as an OTLP value. A reference is a kvlist, which the mapper reads as an
// object, so the payload holds the same reference whichever spelling the
// value arrived in.
func otlpValue(v any) *commonpb.AnyValue {
	ref, isRef := v.(map[string]any)
	if !isRef {
		s, _ := v.(string)
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	list := &commonpb.KeyValueList{}
	add := func(key string, value *commonpb.AnyValue) {
		list.Values = append(list.Values, &commonpb.KeyValue{Key: key, Value: value})
	}
	add(MediaRefKey, otlpValue(ref[MediaRefKey]))
	add(mediaMimeKey, otlpValue(ref[mediaMimeKey]))
	if size, ok := ref[mediaSizeKey].(int64); ok {
		add(mediaSizeKey, &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: size}})
	}
	if stored, ok := ref[mediaStoredKey].(bool); ok {
		add(mediaStoredKey, &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: stored}})
	}
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: list}}
}
