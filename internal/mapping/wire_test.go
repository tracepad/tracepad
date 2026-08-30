package mapping_test

import (
	"math"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/otlptest"
)

// What the wire already carries (spec 012): the type vocabulary, the
// completion start, the prompt link, the release, and the origin every
// unclaimed attribute keeps in metadata.

// The ten types are stored as sent. Before this spec the column held three of
// them and the mapper collapsed the rest onto the nearest one, which made
// `agent`, `tool` and `guardrail` all read as `span` (spec 012 #2).
func TestObservationTypeVocabulary(t *testing.T) {
	for _, want := range model.ObservationTypes {
		t.Run(want, func(t *testing.T) {
			result := mapping.Map(otlptest.SpanWith("langfuse.observation.type", want))
			observation := result.Observations[0]
			if observation.Type != want {
				t.Errorf("type = %q, want %q stored as sent", observation.Type, want)
			}
			// The spelling was kept in metadata only because the
			// column could not hold it; now that it can, keeping a
			// copy would be one fact in two places.
			if _, duplicated := observation.Metadata["langfuse.observation.type"]; duplicated {
				t.Errorf("metadata = %v, want no copy of a type the column now holds",
					observation.Metadata)
			}
		})
	}
}

// An embedding is a model call and used to be filed as a generation. It is
// its own type now, and every aggregate that sums model calls has to keep
// counting it (spec 012 #2) — which the store tests assert on the rows this
// one produces.
func TestEmbeddingNoLongerCollapses(t *testing.T) {
	observation := mapping.Map(otlptest.SpanWith(
		"langfuse.observation.type", "embedding",
		"langfuse.observation.model.name", "text-embedding-3-large",
	)).Observations[0]

	if observation.Type != model.TypeEmbedding {
		t.Errorf("type = %q, want %q", observation.Type, model.TypeEmbedding)
	}
	if observation.Model == "" {
		t.Error("an embedding is a call to a model and must keep the model column")
	}
}

// A spelling outside the ten is still something the column cannot hold, so it
// is preserved in metadata and the heuristics decide the type (spec 002 #12).
func TestUnknownTypeSpellingFallsToHeuristics(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{
			name: "a model attribute means a generation",
			keys: []string{
				"langfuse.observation.type", "workflow-step",
				"gen_ai.request.model", "gpt-4o-mini",
			},
			want: model.TypeGeneration,
		},
		{
			name: "everything else is a span",
			keys: []string{"langfuse.observation.type", "workflow-step"},
			want: model.TypeSpan,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			observation := mapping.Map(otlptest.SpanWith(c.keys...)).Observations[0]
			if observation.Type != c.want {
				t.Errorf("type = %q, want %q", observation.Type, c.want)
			}
			if observation.Metadata["langfuse.observation.type"] != "workflow-step" {
				t.Errorf("metadata = %v, want the unknown spelling preserved",
					observation.Metadata)
			}
		})
	}
}

// A zero-duration childless span is still an event, which is the heuristic a
// widened vocabulary must not have disturbed.
func TestZeroDurationSpanIsStillAnEvent(t *testing.T) {
	span := otlptest.ProbeSpan()
	span.EndTimeUnixNano = span.StartTimeUnixNano
	if observation := mapping.Map(otlptest.Export(span)).Observations[0]; observation.Type != model.TypeEvent {
		t.Errorf("type = %q, want %q", observation.Type, model.TypeEvent)
	}
}

// The completion start arrives in three shapes and is claimed only for the
// ones that parse. The double-encoded string is what the SDK 4.7 puts on the
// wire, and accepting it is the difference between a feature and a metadata
// entry (spec 012 #3).
func TestCompletionStartTime(t *testing.T) {
	const start = 1787738400_000_000_000 // the fixtures' base instant

	cases := []struct {
		name     string
		value    *commonpb.AnyValue
		want     int64
		inMetada bool
	}{
		{
			name:  "RFC 3339",
			value: stringValue("2026-08-26T10:00:00.488Z"),
			want:  start + 488_000_000,
		},
		{
			name:  "RFC 3339 with a layer of JSON quoting left on",
			value: stringValue(`"2026-08-26T10:00:00.488Z"`),
			want:  start + 488_000_000,
		},
		{
			name:  "RFC 3339 with an offset rather than Z",
			value: stringValue("2026-08-26T12:00:00.488+02:00"),
			want:  start + 488_000_000,
		},
		{
			name:  "integer nanoseconds",
			value: intValue(start + 250_000_000),
			want:  start + 250_000_000,
		},
		{
			name:  "nanoseconds as a decimal string",
			value: stringValue("1787738400250000000"),
			want:  start + 250_000_000,
		},
		{
			name:     "a value that is neither",
			value:    stringValue("as soon as it could"),
			inMetada: true,
		},
		{
			name:     "a number that is not whole",
			value:    doubleValue(1.5),
			inMetada: true,
		},
		// A double is whole at any magnitude, so the integrality check
		// alone let this through, and converting it to int64 is
		// implementation-defined: it would be stored as an instant and
		// subtracted into a TTFT of some 10^12 milliseconds (found in
		// review of PR #19).
		{
			name:     "a whole number past what int64 holds",
			value:    doubleValue(1e30),
			inMetada: true,
		},
		// The infinities reach this rule as text rather than as doubles,
		// since JSON cannot spell one — the point of asserting them here
		// is that the two screenings agree on where the value ends up.
		{
			name:     "a number that is not finite",
			value:    doubleValue(math.Inf(1)),
			inMetada: true,
		},
		{
			name:     "not a number at all",
			value:    doubleValue(math.NaN()),
			inMetada: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			span := otlptest.ProbeSpan()
			span.Attributes = []*commonpb.KeyValue{{
				Key:   "langfuse.observation.completion_start_time",
				Value: c.value,
			}}
			observation := mapping.Map(otlptest.Export(span)).Observations[0]

			if observation.CompletionStartTime != c.want {
				t.Errorf("completion_start_time = %d, want %d",
					observation.CompletionStartTime, c.want)
			}
			_, kept := observation.Metadata["langfuse.observation.completion_start_time"]
			if kept != c.inMetada {
				t.Errorf("in metadata = %v, want %v: a value the column took is claimed, "+
					"a value it refused must stay visible (spec 002 #11)", kept, c.inMetada)
			}
		})
	}
}

// A completion start earlier than the span's own start is stored as sent: no
// clamping and no clock correction (spec 002 #4, spec 012 edge cases).
func TestCompletionStartBeforeTheSpanStarted(t *testing.T) {
	observation := mapping.Map(otlptest.SpanWith(
		"langfuse.observation.completion_start_time", "2026-08-26T09:59:59Z",
	)).Observations[0]

	if observation.CompletionStartTime >= observation.StartTime {
		t.Fatalf("completion_start_time = %d, start_time = %d: the value must arrive unclamped",
			observation.CompletionStartTime, observation.StartTime)
	}
}

// The prompt link records what the client said. The name and the version are
// independent: "which prompt" is the question the filter answers, and half an
// answer beats none (spec 012 #5).
func TestPromptLink(t *testing.T) {
	t.Run("name and version", func(t *testing.T) {
		span := otlptest.ProbeSpan("langfuse.observation.prompt.name", "support-answer")
		span.Attributes = append(span.Attributes, &commonpb.KeyValue{
			Key: "langfuse.observation.prompt.version", Value: intValue(7),
		})
		observation := mapping.Map(otlptest.Export(span)).Observations[0]

		if observation.PromptName != "support-answer" {
			t.Errorf("prompt_name = %q", observation.PromptName)
		}
		if observation.PromptVersion == nil || *observation.PromptVersion != 7 {
			t.Errorf("prompt_version = %v, want 7", observation.PromptVersion)
		}
		for _, key := range []string{
			"langfuse.observation.prompt.name",
			"langfuse.observation.prompt.version",
		} {
			if _, kept := observation.Metadata[key]; kept {
				t.Errorf("metadata[%q] is set, want both attributes claimed", key)
			}
		}
	})

	t.Run("a version that is not an integer", func(t *testing.T) {
		observation := mapping.Map(otlptest.SpanWith(
			"langfuse.observation.prompt.name", "catalog-query",
			"langfuse.observation.prompt.version", "latest",
		)).Observations[0]

		if observation.PromptName != "catalog-query" {
			t.Errorf("prompt_name = %q, want the name recorded anyway", observation.PromptName)
		}
		if observation.PromptVersion != nil {
			t.Errorf("prompt_version = %v, want none", *observation.PromptVersion)
		}
		if observation.Metadata["langfuse.observation.prompt.version"] != "latest" {
			t.Errorf("metadata = %v, want the unusable version preserved", observation.Metadata)
		}
	})

	t.Run("a version without a name", func(t *testing.T) {
		observation := mapping.Map(otlptest.SpanWith(
			"langfuse.observation.prompt.version", "7",
		)).Observations[0]

		if observation.PromptName != "" || observation.PromptVersion != nil {
			t.Errorf("prompt = %q@%v, want none: a version labels nothing on its own",
				observation.PromptName, observation.PromptVersion)
		}
		if observation.Metadata["langfuse.observation.prompt.version"] != "7" {
			t.Errorf("metadata = %v, want it preserved", observation.Metadata)
		}
	})
}

// `service.version` is the OTel resource attribute a plain-OTel app already
// sets, which is what makes the release filter work for people who never
// heard of the dialect (spec 012 #4).
func TestReleaseFromServiceVersion(t *testing.T) {
	result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
		Resource:  []string{"service.name", "pricing", "service.version", "1.9.0"},
		ScopeName: "probe", ScopeVersion: "0.0.0",
		Spans: [][]string{nil},
	}))

	if release := result.Traces[0].Release; release != "1.9.0" {
		t.Errorf("release = %q, want the resource's service.version", release)
	}
	metadata := result.Observations[0].Metadata
	if _, kept := metadata["resource.service.version"]; kept {
		t.Errorf("metadata = %v, want service.version claimed once it is the release", metadata)
	}
}

// The observation's own version is not the trace's, and stays where the
// mapper found it (spec 012 #4).
func TestObservationVersionStaysInMetadata(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith("langfuse.observation.version", "answer-3"))
	if version := result.Traces[0].Version; version != "" {
		t.Errorf("trace version = %q, want none", version)
	}
	if result.Observations[0].Metadata["langfuse.observation.version"] != "answer-3" {
		t.Errorf("metadata = %v, want it preserved", result.Observations[0].Metadata)
	}
}

// Provenance (spec 012 #7): the three levels used to be one flat namespace,
// so a resource `service.name` and a span attribute of the same name were
// indistinguishable and whichever merged last was the only one left. Now each
// keeps the name its origin gives it.
func TestProvenanceKeepsEveryLevel(t *testing.T) {
	result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
		Resource:  []string{"service.name", "from-resource"},
		Scope:     []string{"service.name", "from-scope"},
		ScopeName: "langfuse-sdk", ScopeVersion: "4.7.0",
		Spans: [][]string{{"service.name", "from-span"}},
	}))

	metadata := result.Observations[0].Metadata
	for key, want := range map[string]string{
		"resource.service.name": "from-resource",
		"scope.service.name":    "from-scope",
		"service.name":          "from-span",
		"scope.name":            "langfuse-sdk",
		"scope.version":         "4.7.0",
	} {
		if metadata[key] != want {
			t.Errorf("metadata[%q] = %#v, want %q", key, metadata[key], want)
		}
	}
}

// A mapping chain still reads all three levels by bare key: a rule that wants
// a resource attribute should not have to know it is one (spec 012 #7).
func TestChainsStillReadEveryLevelByBareKey(t *testing.T) {
	result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
		Resource:  []string{"deployment.environment", "prod"},
		ScopeName: "probe", ScopeVersion: "0.0.0",
		Spans: [][]string{nil},
	}))

	if environment := result.Traces[0].Environment; environment != "prod" {
		t.Errorf("environment = %q, want the resource attribute to have set it", environment)
	}
	metadata := result.Observations[0].Metadata
	if _, leaked := metadata["resource.deployment.environment"]; leaked {
		t.Errorf("metadata = %v, want a claimed attribute to stay out of it", metadata)
	}
}

// The scope's name and version are not attributes at all in OTLP, so they are
// absent when the scope did not carry them rather than present and empty.
func TestScopeIdentityIsOptional(t *testing.T) {
	result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
		Spans: [][]string{{"langfuse.observation.type", "span"}},
	}))

	metadata := result.Observations[0].Metadata
	for _, key := range []string{"scope.name", "scope.version"} {
		if _, present := metadata[key]; present {
			t.Errorf("metadata[%q] = %#v, want it absent for a scope that named itself nothing",
				key, metadata[key])
		}
	}
}

// A scope attribute literally called `name` cannot hide the scope's own name:
// the identity is written after the attributes, exactly as the event list is
// (spec 012 #7).
func TestScopeIdentityBeatsAnAttributeOfTheSameName(t *testing.T) {
	result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
		Scope:     []string{"name", "an-attribute"},
		ScopeName: "langfuse-sdk", ScopeVersion: "4.7.0",
		Spans: [][]string{nil},
	}))

	if got := result.Observations[0].Metadata["scope.name"]; got != "langfuse-sdk" {
		t.Errorf("scope.name = %#v, want the instrumentation scope's own name", got)
	}
}

// A chain resolved per span but merged per trace loses its priority unless
// the merge knows the rank: `langfuse.release` on the root and
// `service.version` on the resource — which every span can see — used to
// store the fallback on any trace longer than one span (spec 012 #11).
func TestChainPrioritySurvivesTheTraceMerge(t *testing.T) {
	cases := []struct {
		name     string
		resource []string
		root     []string
		read     func(*model.Trace) string
		want     string
	}{
		{
			name:     "release",
			resource: []string{"service.version", "2026.8.3"},
			root:     []string{"langfuse.release", "2026.8.30-rc1"},
			read:     func(t *model.Trace) string { return t.Release },
			want:     "2026.8.30-rc1",
		},
		{
			name:     "environment",
			resource: []string{"deployment.environment", "prod"},
			root:     []string{"langfuse.environment", "production"},
			read:     func(t *model.Trace) string { return t.Environment },
			want:     "production",
		},
		{
			name:     "user id",
			resource: []string{"user.id", "resource-user"},
			root:     []string{"langfuse.user.id", "user-4821"},
			read:     func(t *model.Trace) string { return t.UserID },
			want:     "user-4821",
		},
		{
			name:     "session id",
			resource: []string{"session.id", "resource-session"},
			root:     []string{"langfuse.session.id", "session-77"},
			read:     func(t *model.Trace) string { return t.SessionID },
			want:     "session-77",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Three spans: the explicit key on the root only, the
			// resource's fallback visible to all of them.
			result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
				Resource:  c.resource,
				ScopeName: "langfuse-sdk", ScopeVersion: "4.7.0",
				Spans: [][]string{c.root, nil, nil},
			}))
			if got := c.read(result.Traces[0]); got != c.want {
				t.Errorf("%s = %q, want %q: the fallback must not beat the explicit key",
					c.name, got, c.want)
			}
		})
	}
}

// At equal rank the later span still wins, which is spec 002 #6 and the right
// rule for two spans that genuinely disagree about the same key.
func TestEqualRankKeepsTheLastValue(t *testing.T) {
	result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
		ScopeName: "langfuse-sdk", ScopeVersion: "4.7.0",
		Spans: [][]string{
			{"langfuse.release", "first"},
			{"langfuse.release", "second"},
		},
	}))

	if release := result.Traces[0].Release; release != "second" {
		t.Errorf("release = %q, want the later delivery of the same key", release)
	}
}

// A span with no attributes at all contributes nothing rather than clearing
// what its siblings set.
func TestSpansWithoutTheFieldContributeNothing(t *testing.T) {
	result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
		ScopeName: "langfuse-sdk", ScopeVersion: "4.7.0",
		Spans: [][]string{{"langfuse.version", "checkout-v9"}, nil},
	}))

	if version := result.Traces[0].Version; version != "checkout-v9" {
		t.Errorf("version = %q, want the one span that named it to have won", version)
	}
}

func stringValue(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func intValue(n int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
}

func doubleValue(f float64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
}
