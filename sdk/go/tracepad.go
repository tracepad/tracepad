package tracepad

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Option configures Init.
type Option func(*options)

type options struct {
	host, key, environment, release string
	export                          bool
	logger                          *slog.Logger
	provider                        trace.TracerProvider
}

// WithHost names the store, e.g. "http://localhost:4318". TRACEPAD_HOST
// otherwise.
func WithHost(host string) Option { return func(o *options) { o.host = host } }

// WithKey is a secret key ("tp-sk-…"), sent as Bearer. TRACEPAD_API_KEY
// otherwise.
func WithKey(key string) Option { return func(o *options) { o.key = key } }

// WithEnvironment names the deployment this process is; it lands on the
// resource as deployment.environment.name. TRACEPAD_ENVIRONMENT otherwise.
func WithEnvironment(environment string) Option {
	return func(o *options) { o.environment = environment }
}

// WithRelease is the version of the application's own logic; it lands on
// the resource as service.version. TRACEPAD_RELEASE otherwise.
func WithRelease(release string) Option { return func(o *options) { o.release = release } }

// WithExport(false) attaches everything except the exporter, for an
// application whose traces already reach the store another way.
func WithExport(export bool) Option { return func(o *options) { o.export = export } }

// WithLogger is where the tracing path says what it could not do (spec 017
// #9). slog.Default() otherwise.
func WithLogger(logger *slog.Logger) Option { return func(o *options) { o.logger = logger } }

// WithTracerProvider names the provider to attach to and to start spans on,
// instead of the global one — what a test and the rare two-destination
// application use. Init neither sets it global nor replaces it.
func WithTracerProvider(provider trace.TracerProvider) Option {
	return func(o *options) { o.provider = provider }
}

// defaults is the one default the package keeps (spec 033 #2): a span helper
// that needed a receiver on every call would not be a helper.
type defaults struct {
	mu          sync.Mutex
	initialized bool
	config      *config
	logger      *slog.Logger
	// provider is the one Init was handed explicitly; nil means the global
	// one, read at every call so that it is whatever the application set.
	provider trace.TracerProvider
	// sdk is the SDK provider the processors were registered on — adopted
	// or built — and what Flush asks to flush; built says which.
	sdk      *sdktrace.TracerProvider
	built    bool
	scores   *scoreQueue
	shutdown func(context.Context) error
}

var def = &defaults{}

func (d *defaults) log() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

// Init points this process at a Tracepad store (spec 033 #2). It returns the
// function that flushes everything queued and, when Init built the provider,
// shuts it down: call it where the application closes the rest.
//
// The call adapts to the provider it finds. When the global TracerProvider
// is the SDK's — otelhttp, otelgrpc, another SDK set it — Init registers a
// batching OTLP/HTTP exporter on it, so our spans and theirs share one
// pipeline and one flush. Only when the global is the API's no-op default
// does Init build one and set it, with a TraceContext propagator, which is
// what makes the one-liner true for a script.
//
// With no host or key in the options or the environment the error is
// ErrConfig. A second Init is a no-op with a warning; it hands back the
// first call's shutdown function.
func Init(ctx context.Context, opts ...Option) (shutdown func(context.Context) error, err error) {
	o := options{export: true}
	for _, opt := range opts {
		opt(&o)
	}
	d := def
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.initialized {
		d.log().Warn("tracepad.Init has already run; this call is a no-op")
		return d.shutdown, nil
	}
	c, err := resolve(o.host, o.key, o.environment, o.release)
	if err != nil {
		return nil, err
	}
	if o.logger != nil {
		d.logger = o.logger
	}
	provider := o.provider
	if provider == nil {
		provider = otel.GetTracerProvider()
	}
	sdk, _ := provider.(*sdktrace.TracerProvider)
	built := false
	switch {
	case sdk != nil && (c.environment != "" || c.release != ""):
		// A resource is fixed when its provider is built, and service.version
		// is read from the resource only (docs/ingest.md), so neither can be
		// added to somebody else's provider afterwards.
		d.log().Warn("tracepad.Init: environment and release are resource attributes and this " +
			"process already has a TracerProvider; set deployment.environment.name and " +
			"service.version on its resource (OTEL_RESOURCE_ATTRIBUTES) instead")
	case sdk == nil && o.provider == nil && isDefault(provider):
		sdk = sdktrace.NewTracerProvider(sdktrace.WithResource(newResource(c)))
		otel.SetTracerProvider(sdk)
		otel.SetTextMapPropagator(propagation.TraceContext{})
		provider, built = sdk, true
	case sdk == nil:
		d.log().Warn("tracepad.Init: the TracerProvider is not the OpenTelemetry SDK's; " +
			"spans are started on it, but no exporter can be attached to it")
	}
	if sdk != nil {
		if err := attach(ctx, sdk, c, o.export); err != nil {
			return nil, err
		}
	}
	d.config = &c
	d.provider = o.provider
	d.sdk, d.built = sdk, built
	// A score written before Init already made the queue; it is kept, so
	// that what it holds is sent and its goroutine is the one Flush drains.
	if d.scores == nil {
		d.scores = newScoreQueue(postScores)
	}
	d.scores.start()
	d.initialized = true
	d.shutdown = func(ctx context.Context) error {
		return d.close(ctx)
	}
	return d.shutdown, nil
}

// attach registers what the package adds to an SDK provider: the exporter,
// under a batching processor, unless the application exports another way.
func attach(ctx context.Context, sdk *sdktrace.TracerProvider, c config, export bool) error {
	if !export {
		return nil
	}
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(c.host+"/v1/traces"),
		otlptracehttp.WithHeaders(map[string]string{"Authorization": "Bearer " + c.key}))
	if err != nil {
		return fmt.Errorf("tracepad: building the exporter: %w", err)
	}
	sdk.RegisterSpanProcessor(sdktrace.NewBatchSpanProcessor(exporter))
	return nil
}

// isDefault tells the API's own delegating provider — the one every process
// has until something sets one — from a provider an application set that is
// not the SDK's. There is no public way to ask, and replacing an
// application's provider because its type was unfamiliar is exactly what
// Decision 2 refuses, so the package's own internal path is the test.
func isDefault(provider trace.TracerProvider) bool {
	t := reflect.TypeOf(provider)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t != nil && t.PkgPath() == "go.opentelemetry.io/otel/internal/global"
}

// newResource is the resource of a provider Init builds: the SDK's default —
// which already read OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES — with
// the executable's name where neither named the service, and the
// environment and the release from the configuration.
func newResource(c config) *resource.Resource {
	base := resource.Default()
	var attrs []attribute.KeyValue
	if name, ok := base.Set().Value(attrServiceName); !ok || strings.HasPrefix(name.AsString(), "unknown_service") {
		attrs = append(attrs, attribute.String(attrServiceName, processName()))
	}
	if c.environment != "" {
		attrs = append(attrs, attribute.String(attrEnvironment, c.environment))
	}
	if c.release != "" {
		attrs = append(attrs, attribute.String(attrServiceVersion, c.release))
	}
	merged, err := resource.Merge(base, resource.NewSchemaless(attrs...))
	if err != nil {
		return resource.NewSchemaless(append(base.Attributes(), attrs...)...)
	}
	return merged
}

func processName() string {
	if exe, err := os.Executable(); err == nil {
		if name := filepath.Base(exe); name != "" && name != "." {
			return name
		}
	}
	return "go"
}

// tracer is where the package's spans come from: the explicit provider, or
// the global one as it is at the moment of the call.
func tracer() trace.Tracer {
	provider := def.provider
	if provider == nil {
		provider = otel.GetTracerProvider()
	}
	return provider.Tracer("tracepad", trace.WithInstrumentationVersion(Version))
}

// Flush delivers everything queued: the scores, then the spans, within the
// context's deadline. Both halves are attempted whatever the first said, and
// the error is theirs joined.
func Flush(ctx context.Context) error {
	d := def
	d.mu.Lock()
	scores, sdk := d.scores, d.sdk
	d.mu.Unlock()
	var err error
	if scores != nil {
		err = scores.flush(ctx)
	}
	if sdk == nil {
		sdk, _ = otel.GetTracerProvider().(*sdktrace.TracerProvider)
	}
	if sdk != nil {
		err = errors.Join(err, sdk.ForceFlush(ctx))
	}
	return err
}

// close is the shutdown Init returns: it flushes, stops the score goroutine,
// and shuts the provider down only when the package built it — an adopted
// provider is the application's to close.
func (d *defaults) close(ctx context.Context) error {
	err := Flush(ctx)
	d.mu.Lock()
	scores, sdk, built := d.scores, d.sdk, d.built
	d.mu.Unlock()
	if scores != nil {
		scores.close()
	}
	if built && sdk != nil {
		err = errors.Join(err, sdk.Shutdown(ctx))
	}
	return err
}

// reset forgets the default, for tests: Init is process-wide by design. The
// queue is closed outside the lock: its goroutine reads the configuration
// under it.
func reset() {
	d := def
	d.mu.Lock()
	scores := d.scores
	d.initialized, d.built = false, false
	d.config, d.logger, d.provider, d.sdk, d.scores, d.shutdown = nil, nil, nil, nil, nil, nil
	d.mu.Unlock()
	if scores != nil {
		scores.close()
	}
	forgetPrompts()
}
