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
		opts:     opts,
		res:      &MediaResult{},
		bodies:   map[string]bool{},
		refs:     map[MediaRef]bool{},
		resolved: map[string]resolvedMedia{},
	}
	w.res.Changed = rewriteMedia(resourceSpans, w)
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
	// rewrite the object in place through its fields (changed), answer a
	// string that replaces the object whole, and stop the walk entering it.
	object(f fields) (replacement string, changed, stop bool)
	// decodes is the cheap look at a string before it is decoded as a JSON
	// document to walk.
	decodes(s string) bool
}

// rewriteMedia runs a rewrite over every attribute of an export — resource,
// scope, span and event — and reports, per ResourceSpans, whether it changed
// anything.
func rewriteMedia(resourceSpans []*tracepb.ResourceSpans, r mediaRewrite) []bool {
	changed := make([]bool, len(resourceSpans))
	for i, rs := range resourceSpans {
		if rs == nil {
			continue
		}
		visit := func(traces []string, kvs []*commonpb.KeyValue) {
			r.enter(traces)
			if rewriteAttributes(r, kvs) {
				changed[i] = true
			}
		}
		if resource := rs.GetResource(); resource != nil {
			visit(tracesOf(rs.GetScopeSpans()...), resource.Attributes)
		}
		for _, ss := range rs.GetScopeSpans() {
			if ss == nil {
				continue
			}
			if scope := ss.GetScope(); scope != nil {
				visit(tracesOf(ss), scope.Attributes)
			}
			for _, span := range ss.GetSpans() {
				if span == nil {
					continue
				}
				var traces []string
				if id := traceID(span.GetTraceId()); id != "" {
					traces = []string{id}
				}
				visit(traces, span.Attributes)
				for _, event := range span.GetEvents() {
					if event != nil {
						visit(traces, event.Attributes)
					}
				}
			}
		}
	}
	return changed
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

func rewriteAttributes(r mediaRewrite, kvs []*commonpb.KeyValue) bool {
	changed := false
	for _, kv := range kvs {
		if kv != nil && rewriteAnyValue(r, kv.Value) {
			changed = true
		}
	}
	return changed
}

// rewriteAnyValue rewrites one OTLP value in place and reports whether it did.
func rewriteAnyValue(r mediaRewrite, v *commonpb.AnyValue) bool {
	if v == nil {
		return false
	}
	switch value := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		if ref, ok := r.whole(value.StringValue); ok {
			v.Value = otlpValue(ref).Value
			return true
		}
		if !r.decodes(value.StringValue) {
			return false
		}
		rewritten, ok := rewriteDocument(r, value.StringValue)
		if ok {
			value.StringValue = rewritten
		}
		return ok
	case *commonpb.AnyValue_ArrayValue:
		changed := false
		for _, item := range value.ArrayValue.GetValues() {
			if rewriteAnyValue(r, item) {
				changed = true
			}
		}
		return changed
	case *commonpb.AnyValue_KvlistValue:
		kvs := value.KvlistValue.GetValues()
		replacement, changed, stop := r.object(kvlistFields(kvs))
		if replacement != "" {
			v.Value = &commonpb.AnyValue_StringValue{StringValue: replacement}
			return true
		}
		if stop {
			return changed
		}
		return rewriteAttributes(r, kvs) || changed
	}
	return false
}

// rewriteDocument is a string attribute that holds a JSON document: decoded,
// walked, and re-encoded only when something in it was replaced. A document
// that was, whole, one reference goes back to the string it replaced rather
// than to that string's JSON encoding.
func rewriteDocument(r mediaRewrite, s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return "", false
	}
	document, ok := decodeDocument(trimmed)
	if !ok {
		return "", false
	}
	rewritten, changed := rewriteJSON(r, document)
	if !changed {
		return "", false
	}
	if text, isText := rewritten.(string); isText {
		return text, true
	}
	return encodeDocument(rewritten)
}

// rewriteJSON walks a decoded document, answering its replacement.
func rewriteJSON(r mediaRewrite, v any) (any, bool) {
	switch value := v.(type) {
	case string:
		if ref, ok := r.whole(value); ok {
			return ref, true
		}
	case []any:
		changed := false
		for i, item := range value {
			if replacement, ok := rewriteJSON(r, item); ok {
				value[i] = replacement
				changed = true
			}
		}
		return value, changed
	case map[string]any:
		replacement, changed, stop := r.object(mapFields(value))
		if replacement != "" {
			return replacement, true
		}
		if stop {
			return value, changed
		}
		for key, item := range value {
			if replacement, ok := rewriteJSON(r, item); ok {
				value[key] = replacement
				changed = true
			}
		}
		return value, changed
	}
	return v, false
}

// fields reads and writes the fields of an object over either spelling of
// one: a decoded JSON map or an OTLP kvlist. A value set is a string or a
// reference object.
type fields struct {
	has    func(key string) bool
	text   func(key string) (string, bool)
	flag   func(key string) (bool, bool)
	nested func(key string) (fields, bool)
	set    func(key string, value any)
}

func mapFields(m map[string]any) fields {
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
			return mapFields(inner), true
		},
		set: func(key string, value any) { m[key] = value },
	}
}

func kvlistFields(kvs []*commonpb.KeyValue) fields {
	find := func(key string) *commonpb.KeyValue {
		for _, kv := range kvs {
			if kv != nil && kv.Key == key {
				return kv
			}
		}
		return nil
	}
	return fields{
		has: func(key string) bool { return find(key) != nil },
		text: func(key string) (string, bool) {
			v, ok := find(key).GetValue().GetValue().(*commonpb.AnyValue_StringValue)
			if !ok {
				return "", false
			}
			return v.StringValue, true
		},
		flag: func(key string) (bool, bool) {
			v, ok := find(key).GetValue().GetValue().(*commonpb.AnyValue_BoolValue)
			if !ok {
				return false, false
			}
			return v.BoolValue, true
		},
		nested: func(key string) (fields, bool) {
			v, ok := find(key).GetValue().GetValue().(*commonpb.AnyValue_KvlistValue)
			if !ok {
				return fields{}, false
			}
			return kvlistFields(v.KvlistValue.GetValues()), true
		},
		set: func(key string, value any) {
			if kv := find(key); kv != nil {
				kv.Value = otlpValue(value)
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
func (w *mediaWalk) object(f fields) (string, bool, bool) {
	holder, key, mime, ok := mediaSlot(f)
	if !ok {
		return "", false, false
	}
	data, isText := holder.text(key)
	if !isText {
		return "", false, false
	}
	var ref map[string]any
	if _, id, isRef := parseLangfuseRef(data); isRef {
		ref = w.langfuse(mime, id)
	} else {
		ref = w.reference(mime, data)
	}
	if ref == nil {
		return "", false, true
	}
	holder.set(key, ref)
	return "", true, true
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
	if w.opts.Resolve == nil {
		return nil
	}
	found, asked := w.resolved[id]
	if !asked {
		found.sha, found.size, found.ok = w.opts.Resolve(id)
		w.resolved[id] = found
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
