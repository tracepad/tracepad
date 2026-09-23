package tracepad_test

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	tracepad "github.com/tracepad/tracepad/sdk/go"
)

// The examples of docs/sdk-go.md, as go vet compiles them (spec 033,
// Testing). None of them runs: they need a store and a model, and what the
// suite proves is that the page's code is the package's API. The client
// below stands in for openai-go's, with the fields the page reads.

type client struct{}

type response struct {
	Model   string
	Usage   struct{ PromptTokens, CompletionTokens int64 }
	Choices []struct{ Message struct{ Content string } }
}

type chunk struct {
	Choices []struct{ Delta struct{ Content string } }
}

type stream struct{}

func (stream) Next() bool     { return false }
func (stream) Current() chunk { return chunk{} }
func (stream) Err() error     { return nil }

func (client) New(context.Context, any) (*response, error) { return &response{}, nil }
func (client) NewStreaming(context.Context, any) stream    { return stream{} }
func retrieve(context.Context, string) ([]string, error)   { return nil, nil }
func text(r *response) string                              { return r.Choices[0].Message.Content }
func usage(r *response) tracepad.Usage                     { return tracepad.Usage{"input_tokens": r.Usage.PromptTokens} }
func params() any                                          { return nil }

func Example() {
	ctx := context.Background()
	shutdown, err := tracepad.Init(ctx) // TRACEPAD_HOST, TRACEPAD_API_KEY
	if err != nil {
		log.Fatal(err)
	}
	defer shutdown(ctx)
}

// answer is the quickstart's function, and the README's.
func answer(ctx context.Context, question string) (string, error) {
	ctx, step := tracepad.Span(ctx, "answer", tracepad.WithInput(question))
	defer step.End()
	tracepad.UpdateTrace(ctx, tracepad.WithUserID("u-42"), tracepad.WithTags("support"))

	ctx, call := tracepad.Generation(ctx, "chat", tracepad.WithModel("gpt-4o-mini"))
	response, err := client{}.New(ctx, params())
	if err != nil {
		call.Fail(err)
		return "", err
	}
	call.End(tracepad.Result{Model: response.Model, Usage: usage(response), Output: text(response)})
	_ = tracepad.Score(ctx, "helpful", tracepad.WithValue(1)) // against the trace in flight
	return text(response), nil
}

func ExampleSpan() {
	ctx := context.Background()
	ctx, step := tracepad.Span(ctx, "answer", tracepad.WithInput("question"))
	defer step.End()

	documents, err := retrieve(ctx, "question")
	if err != nil {
		step.Fail(err)
		return
	}
	step.Update(tracepad.WithOutput(documents), tracepad.WithMetadata(map[string]any{"hits": len(documents)}))

	_, miss := tracepad.Event(ctx, "cache.miss", tracepad.WithMetadata(map[string]any{"key": "k"}))
	miss.End()
}

func ExampleGeneration() {
	ctx := context.Background()
	messages := []tracepad.Message{{Role: "user", Content: "hi"}}
	ctx, call := tracepad.Generation(ctx, "chat",
		tracepad.WithModel("gpt-4o-mini"),
		tracepad.WithModelParameters(map[string]any{"temperature": 0.2}),
		tracepad.WithInput(messages))

	response, err := client{}.New(ctx, params())
	if err != nil {
		call.Fail(err)
		return
	}
	call.End(tracepad.Result{
		Model:  response.Model,
		Usage:  tracepad.Usage{"input_tokens": response.Usage.PromptTokens, "output_tokens": response.Usage.CompletionTokens},
		Output: response.Choices[0].Message.Content,
	})
}

func ExampleCall_FirstToken() {
	ctx := context.Background()
	ctx, call := tracepad.Generation(ctx, "chat", tracepad.WithModel("gpt-4o-mini"))
	stream := client{}.NewStreaming(ctx, params())
	var text strings.Builder
	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) > 0 {
			call.FirstToken()
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if err := stream.Err(); err != nil {
		call.Fail(err)
		return
	}
	call.End(tracepad.Result{Model: "gpt-4o-mini", Output: text.String()})
}

func ExampleUpdateTrace() {
	ctx := context.Background()
	tracepad.UpdateTrace(ctx, tracepad.WithTraceName("support-chat"), tracepad.WithUserID("u-42"),
		tracepad.WithSessionID("s-7"), tracepad.WithTags("support"), tracepad.WithTraceMetadata(map[string]any{"channel": "web"}),
		tracepad.WithTraceVersion("retrieval-v2"))
	tracepad.Update(ctx, tracepad.WithLevel("WARNING"), tracepad.WithStatusMessage("retried once"))
}

func ExampleScore() {
	ctx := context.Background()
	_ = tracepad.Score(ctx, "helpful", tracepad.WithValue(0.9), tracepad.WithComment("cited the source"))
	_ = tracepad.Score(ctx, "verdict", tracepad.WithStringValue("pass"), tracepad.WithDataType("categorical"))
	err := tracepad.Score(ctx, "grounded", tracepad.WithValue(1), tracepad.WithDataType("boolean"), tracepad.OnObservation())
	if errors.Is(err, tracepad.ErrNoTrace) {
		log.Print("no span in the context: pass tracepad.WithTraceID")
	}
}

func ExamplePrompt() {
	ctx := context.Background()
	support, err := tracepad.Prompt(ctx, "support-answer", tracepad.WithLabel("production"))
	if err != nil {
		log.Fatal(err)
	}
	messages := support.Compile(map[string]any{"product": "Tracepad"}).Messages

	ctx, call := tracepad.Generation(ctx, "chat", tracepad.WithPrompt(support),
		tracepad.WithModel(support.Config["model"].(string)), tracepad.WithInput(messages))
	call.End(tracepad.Result{})
}

// ExampleDeleteTraces is docs/sdk-go.md#deleting-traces: the dry run, then
// the project's name as the echo, then one trace by its id.
func ExampleDeleteTraces() {
	ctx := context.Background()
	_, step := tracepad.Span(ctx, "answer")
	step.End()

	filter := tracepad.TraceFilter{To: time.Now(), Environment: "loadtest"}
	preview, err := tracepad.DeleteTraces(ctx, filter, "")
	if err != nil {
		log.Fatal(err)
	}
	log.Println(preview["matched"], preview["would_delete"])
	total, err := tracepad.DeleteTraces(ctx, filter, "checkout-service")
	if err != nil {
		log.Fatal(err)
	}
	log.Println(total["deleted"], total["rounds"])

	_, err = tracepad.DeleteTrace(ctx, step.TraceID(), true)
	if err != nil {
		log.Fatal(err)
	}
}

func judge(answer string, expected any) float64 { return 1 }

// ExampleRun_Item is the loop of docs/sdk-go.md#evals and docs/datasets.md.
func ExampleRun_Item() {
	ctx := context.Background()
	var cases []tracepad.Item
	zero, one := 0.0, 1.0
	_ = tracepad.ScoreConfigs(ctx, []tracepad.ScoreConfig{
		{Name: "accuracy", DataType: "numeric", Direction: "higher", Min: &zero, Max: &one},
		{Name: "verdict", DataType: "categorical", Categories: []string{"pass", "fail"}},
	})

	golden := tracepad.NewDataset("support-golden")
	_, _, _ = golden.PutItems(ctx, cases) // same cases → same version, nothing written

	run, err := golden.Run(ctx, "prompt v7", tracepad.WithRunMetadata(map[string]any{"prompt": "support-answer@7"}))
	if err != nil {
		log.Fatal(err)
	}
	for item, err := range golden.Items(ctx, run.DatasetVersion) {
		if err != nil {
			_, _ = run.Fail(ctx, err)
			log.Fatal(err)
		}
		itemCtx, attempt := run.Item(ctx, item)
		answer, _ := answer(itemCtx, item.Input.(map[string]any)["question"].(string))
		_ = attempt.Score(ctx, "accuracy", tracepad.WithValue(judge(answer, item.ExpectedOutput)))
		_ = attempt.Score(ctx, "verdict", tracepad.WithStringValue("pass"), tracepad.WithDataType("categorical"))
	}
	summary, err := run.Finish(ctx)
	if err != nil {
		log.Fatal(err)
	}
	_ = summary["summary"]
	_, _ = tracepad.Compare(ctx, run.ID, tracepad.ItemID("a natural key"))
}
