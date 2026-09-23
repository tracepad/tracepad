package mapping

import (
	"encoding/base64"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// MediaFetch answers a body by its hash: its stored MIME type and bytes, or
// false when there is none to give.
type MediaFetch func(sha string) (mime string, body []byte, ok bool)

// InlineMedia is ExtractMedia run backwards, for the way out (spec 041 #8): a
// reference becomes a data URL again, so a batch leaving for another backend
// is whole. It reports, per ResourceSpans, whether it rewrote anything.
//
// Every shape comes back as a data URL — the reference does not remember
// whether it replaced an Anthropic source or a string — and that is enough for
// the round trip: posted back to a Tracepad, the data URL is extracted to the
// same reference, and the observation reads the same. A reference with
// `"stored": false`, or whose body is gone, stays a reference.
func InlineMedia(resourceSpans []*tracepb.ResourceSpans, fetch MediaFetch) []bool {
	in := &mediaInline{fetch: fetch, urls: map[string]string{}}
	changed := make([]bool, len(resourceSpans))
	for i, rs := range resourceSpans {
		if rs == nil {
			continue
		}
		touched := false
		if resource := rs.GetResource(); resource != nil {
			touched = in.attributes(resource.Attributes) || touched
		}
		for _, ss := range rs.GetScopeSpans() {
			if scope := ss.GetScope(); scope != nil {
				touched = in.attributes(scope.Attributes) || touched
			}
			for _, span := range ss.GetSpans() {
				if span == nil {
					continue
				}
				touched = in.attributes(span.Attributes) || touched
				for _, event := range span.GetEvents() {
					if event != nil {
						touched = in.attributes(event.Attributes) || touched
					}
				}
			}
		}
		changed[i] = touched
	}
	return changed
}

type mediaInline struct {
	fetch MediaFetch
	// urls caches a body's data URL: the photograph sent to five
	// generations is fetched and encoded once per batch.
	urls map[string]string
}

func (in *mediaInline) attributes(kvs []*commonpb.KeyValue) bool {
	changed := false
	for _, kv := range kvs {
		if kv != nil && in.anyValue(kv.Value) {
			changed = true
		}
	}
	return changed
}

func (in *mediaInline) anyValue(v *commonpb.AnyValue) bool {
	if v == nil {
		return false
	}
	switch value := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		if !strings.Contains(value.StringValue, MediaRefKey) {
			return false
		}
		trimmed := strings.TrimSpace(value.StringValue)
		if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
			return false
		}
		document, ok := decodeDocument(trimmed)
		if !ok {
			return false
		}
		rewritten, changed := in.jsonValue(document)
		if !changed {
			return false
		}
		encoded, ok := encodeDocument(rewritten)
		if !ok {
			return false
		}
		value.StringValue = encoded
		return true
	case *commonpb.AnyValue_ArrayValue:
		changed := false
		for _, item := range value.ArrayValue.GetValues() {
			if in.anyValue(item) {
				changed = true
			}
		}
		return changed
	case *commonpb.AnyValue_KvlistValue:
		kvs := value.KvlistValue.GetValues()
		g := kvlistGetter(kvs)
		if sha, ok := g.text(MediaRefKey); ok {
			mime, _ := g.text(mediaMimeKey)
			if url, ok := in.dataURL(sha, mime, kvlistStored(kvs)); ok {
				v.Value = &commonpb.AnyValue_StringValue{StringValue: url}
				return true
			}
			return false
		}
		return in.attributes(kvs)
	}
	return false
}

func (in *mediaInline) jsonValue(v any) (any, bool) {
	switch value := v.(type) {
	case []any:
		changed := false
		for i, item := range value {
			if replacement, ok := in.jsonValue(item); ok {
				value[i] = replacement
				changed = true
			}
		}
		return value, changed
	case map[string]any:
		if sha, ok := value[MediaRefKey].(string); ok {
			mime, _ := value[mediaMimeKey].(string)
			stored, isBool := value[mediaStoredKey].(bool)
			if url, ok := in.dataURL(sha, mime, !isBool || stored); ok {
				return url, true
			}
			return value, false
		}
		changed := false
		for key, item := range value {
			if replacement, ok := in.jsonValue(item); ok {
				value[key] = replacement
				changed = true
			}
		}
		return value, changed
	}
	return v, false
}

// dataURL renders a body as the data URL a client would have sent. The type
// the reference carries wins over the stored one: it is what this client
// declared (spec 041, edge cases).
func (in *mediaInline) dataURL(sha, mime string, stored bool) (string, bool) {
	if !stored || in.fetch == nil {
		return "", false
	}
	key := sha + "|" + mime
	if url, ok := in.urls[key]; ok {
		return url, true
	}
	storedMime, body, ok := in.fetch(sha)
	if !ok {
		return "", false
	}
	if mime == "" {
		mime = storedMime
	}
	url := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(body)
	in.urls[key] = url
	return url, true
}

func kvlistStored(kvs []*commonpb.KeyValue) bool {
	for _, kv := range kvs {
		if kv != nil && kv.Key == mediaStoredKey {
			if b, ok := kv.Value.GetValue().(*commonpb.AnyValue_BoolValue); ok {
				return b.BoolValue
			}
		}
	}
	return true
}
