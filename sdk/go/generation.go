package tracepad

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Usage is the token counts of a call, by the names the store keeps them
// under: input_tokens, output_tokens, cache_read_input_tokens,
// cache_creation_input_tokens, reasoning_tokens — every entry is written as
// gen_ai.usage.<key>, verbatim.
type Usage map[string]int64

// Result is what a model call came back with (spec 033 #5). Every field is
// optional and an absent one is absent from the span, never zero: a nil Cost
// is "the provider did not say", and the store then shows no data rather
// than $0. The cost is the one you were charged — there is no price table
// here and none in the store.
type Result struct {
	// Model is the one that answered, gen_ai.response.model.
	Model string
	Usage Usage
	// Cost in USD, as the provider reported it; nil when it did not.
	Cost *float64
	// Output is the answer: a string as it is, anything else as JSON.
	Output any
}

// Call is the handle Generation hands out: an observation that called a
// model. (The type is not named Generation because the function is, and Go
// has one namespace for both.)
type Call struct {
	Observation
	firstToken atomic.Bool
}

// WithModel is the model the call asked for, gen_ai.request.model.
func WithModel(model string) GenerationOption {
	return generationOption(func(f *fields) { f.model = model })
}

// WithModelParameters are the request's parameters — temperature,
// max_tokens — each written as gen_ai.request.<key> when the span records.
func WithModelParameters(parameters map[string]any) GenerationOption {
	return generationOption(func(f *fields) { f.parameters = parameters })
}

// WithPrompt records which stored prompt ran, so the trace can be filtered
// by it.
func WithPrompt(prompt *PromptVersion) GenerationOption {
	return generationOption(func(f *fields) { f.prompt = prompt })
}

// Generation opens a call to a model under the context's current span and
// returns the context carrying it. End it with gen.End(Result{…}) — or
// gen.Fail(err), or gen.End(Result{}) to end it with what it has.
func Generation(ctx context.Context, name string, opts ...GenerationOption) (context.Context, *Call) {
	f := fields{typ: attrObservationTypeGenerate}
	for _, opt := range opts {
		opt.applyGeneration(&f)
	}
	var attrs []attribute.KeyValue
	if f.model != "" {
		attrs = append(attrs, attribute.String(attrRequestModel, f.model))
	}
	if f.prompt != nil {
		attrs = append(attrs, attribute.String(attrPromptName, f.prompt.Name),
			attribute.Int(attrPromptVersion, f.prompt.Version))
	}
	g := &Call{}
	return open(ctx, name, &f, &g.Observation, trace.WithAttributes(attrs...)), g
}

// FirstToken stamps the moment the first token came back — where the TTFT
// column comes from. Only the first call counts.
func (g *Call) FirstToken() {
	if g.firstToken.CompareAndSwap(false, true) {
		g.span.SetAttributes(attribute.String(attrCompletionStartTime, rfc3339(time.Now())))
	}
}

// End records the result and ends the span: the model, every Usage entry
// under gen_ai.usage.<key>, the cost and the output. Nothing is written for
// a field left empty.
func (g *Call) End(result Result) {
	if g.ended.Load() {
		return
	}
	var attrs []attribute.KeyValue
	if result.Model != "" {
		attrs = append(attrs, attribute.String(attrResponseModel, result.Model))
	}
	// In key order, so that two exports of one call are one export.
	for _, key := range slices.Sorted(maps.Keys(result.Usage)) {
		attrs = append(attrs, attribute.Int64(attrUsagePrefix+key, result.Usage[key]))
	}
	if result.Cost != nil {
		attrs = append(attrs, attribute.Float64(attrUsageCost, *result.Cost))
	}
	if result.Output != nil && g.span.IsRecording() { // nothing serialised for nothing (spec 042 #1)
		attrs = append(attrs, attribute.String(attrOutput, dumps(result.Output)))
	}
	g.span.SetAttributes(attrs...)
	g.Observation.End()
}

// scalar renders a model parameter or a metadata entry, keeping the types
// OTLP has of its own: every integer width is an int, every float width a
// double (go-openai's temperature is a float32), a bool a bool, a string a
// string, a json.Number the int or the double it spells. Anything else is
// JSON — and a value that encodes as a JSON string, a time.Time, is that
// string without its quotes (found in review of PR #83).
func scalar(key string, value any) attribute.KeyValue {
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Bool:
		return attribute.Bool(key, v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return attribute.Int64(key, v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if v.Uint() <= math.MaxInt64 {
			return attribute.Int64(key, int64(v.Uint()))
		}
	case reflect.Float32, reflect.Float64:
		return attribute.Float64(key, v.Float())
	case reflect.String:
		if number, ok := value.(json.Number); ok {
			if n, err := number.Int64(); err == nil {
				return attribute.Int64(key, n)
			}
			if f, err := number.Float64(); err == nil {
				return attribute.Float64(key, f)
			}
		}
		return attribute.String(key, v.String())
	}
	encoded := dumps(value)
	var text string
	if strings.HasPrefix(encoded, `"`) && json.Unmarshal([]byte(encoded), &text) == nil {
		return attribute.String(key, text)
	}
	return attribute.String(key, encoded)
}
