package tracepad

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/tracepad/tracepad/sdk/go/internal/hook"
)

// Option configures Init.
type Option func(*options)

type options struct {
	host, key, environment, release string
	export                          bool
	exportTimeout                   time.Duration
	logger                          *slog.Logger
	provider                        trace.TracerProvider
}

// String and GoString keep the key out of %v, %+v and %#v, as config's do.
func (o options) String() string {
	o.key = redacted
	type bare options
	return fmt.Sprintf("%+v", bare(o))
}

func (o options) GoString() string { return o.String() }

// WithHost names the store, e.g. "http://localhost:4318". TRACEPAD_URL
// otherwise (TRACEPAD_HOST, deprecated, after that).
func WithHost(host string) Option { return func(o *options) { o.host = host } }

// WithKey is a secret key ("tp-sk-…"), sent as Bearer. TRACEPAD_API_KEY
// otherwise.
func WithKey(key string) Option { return func(o *options) { o.key = key } }

// WithEnvironment names the deployment this process is; it lands on the
// resource as deployment.environment.name. TRACEPAD_ENVIRONMENT otherwise.
func WithEnvironment(environment string) Option {
	return func(o *options) { o.environment = environment }
}

// WithRelease is the version of this deployment; it lands on the resource as
// service.version. TRACEPAD_RELEASE otherwise. The version of one trace's own
// logic — an experiment arm inside a release — is WithTraceVersion.
func WithRelease(release string) Option { return func(o *options) { o.release = release } }

// WithExport(false) attaches everything except the exporter, for an
// application whose traces already reach the store another way.
func WithExport(export bool) Option { return func(o *options) { o.export = export } }

// WithExportTimeout bounds one export, its retries included (spec 042 #3).
// TRACEPAD_EXPORT_TIMEOUT, in seconds, otherwise; then five seconds.
func WithExportTimeout(timeout time.Duration) Option {
	return func(o *options) { o.exportTimeout = timeout }
}

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
	mu sync.Mutex
	// ready is whether Init ran: an atomic, since every span and score reads
	// it while Init holds the lock to build the exporter.
	ready  atomic.Bool
	config *config
	// logger and provider are read on every span and every log line, from
	// any goroutine, while Init may be writing them: atomics, not the lock,
	// which Init holds while it logs.
	logger atomic.Pointer[slog.Logger]
	// provider is the one Init was handed explicitly; unset means the global
	// one, read at every call so that it is whatever the application set.
	provider atomic.Pointer[trace.TracerProvider]
	// sdk is the SDK provider the processors were registered on — adopted
	// or built — and what Flush asks to flush; built says which.
	sdk   *sdktrace.TracerProvider
	built bool
	// propagated says Init set the propagator, which reset takes back.
	propagated bool
	scores     *scoreQueue
	shutdown   func(context.Context) error
}

var def = &defaults{}

func (d *defaults) log() *slog.Logger {
	if logger := d.logger.Load(); logger != nil {
		return logger
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
	if d.ready.Load() {
		d.log().Warn("tracepad.Init has already run; this call is a no-op")
		return d.shutdown, nil
	}
	log := d.log()
	if o.logger != nil {
		log = o.logger
	}
	c, err := resolveWith(log, o.host, o.key, o.environment, o.release)
	if err != nil {
		return nil, err
	}
	return d.start(ctx, c, o)
}

// start is Init past its configuration, under the lock; tracepadtest's
// capture calls it with a configuration of its own (spec 040 #1).
func (d *defaults) start(ctx context.Context, c config, o options) (func(context.Context) error, error) {
	if o.logger != nil {
		d.logger.Store(o.logger)
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
		// The propagator too is set only where nothing was: an application
		// that composed its own (TraceContext and Baggage, say) before Init
		// keeps it, or baggage would silently stop crossing services.
		if len(otel.GetTextMapPropagator().Fields()) == 0 {
			otel.SetTextMapPropagator(propagation.TraceContext{})
			d.propagated = true
		}
		provider, built = sdk, true
	case sdk == nil:
		d.log().Warn("tracepad.Init: the TracerProvider is not the OpenTelemetry SDK's; " +
			"spans are started on it, but no exporter can be attached to it")
	}
	if !o.export && o.exportTimeout != 0 {
		d.log().Warn("tracepad.Init: WithExportTimeout is ignored with WithExport(false), " +
			"which adds no exporter to bound")
	}
	if sdk != nil {
		if err := attach(ctx, sdk, c, o); err != nil {
			return nil, err
		}
	}
	d.config = &c
	if o.provider != nil {
		d.provider.Store(&o.provider)
	}
	d.sdk, d.built = sdk, built
	// A score written before Init already made the queue; it is kept, so
	// that what it holds is sent and its goroutine is the one Flush drains.
	if d.scores == nil {
		d.scores = newScoreQueue(postScores)
	}
	d.scores.start()
	d.ready.Store(true)
	d.shutdown = func(ctx context.Context) error {
		return d.close(ctx)
	}
	return d.shutdown, nil
}

// attach registers what the package adds to an SDK provider: the stamping
// processor of the eval harness first — so that every span the exporter
// batches already carries the run and the item (spec 018 #3), and under
// WithExport(false) too, since an application exporting through another
// SDK still wants its spans stamped — then the exporter, under a batching
// processor, unless the application exports another way.
func attach(ctx context.Context, sdk *sdktrace.TracerProvider, c config, o options) error {
	sdk.RegisterSpanProcessor(runContextProcessor{})
	if !o.export {
		return nil
	}
	exporting := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(c.host + "/v1/traces"),
		otlptracehttp.WithHeaders(map[string]string{"Authorization": "Bearer " + c.key}),
	}
	// The exporter's timeout bounds one attempt, and a timed-out attempt is
	// retried for up to a minute; the batch processor's bounds the export,
	// its retries included, which is what the option promises.
	var batching []sdktrace.BatchSpanProcessorOption
	if timeout := exportTimeout(o.exportTimeout); timeout > 0 {
		exporting = append(exporting, otlptracehttp.WithTimeout(timeout))
		if os.Getenv("OTEL_BSP_EXPORT_TIMEOUT") == "" { // the operator's, where set
			batching = append(batching, sdktrace.WithExportTimeout(timeout))
		}
	}
	exporter, err := otlptracehttp.New(ctx, exporting...)
	if err != nil {
		return fmt.Errorf("tracepad: building the exporter: %w", err)
	}
	sdk.RegisterSpanProcessor(sdktrace.NewBatchSpanProcessor(exporter, batching...))
	return nil
}

// defaultExportTimeout is one export's bound when nothing names one (spec
// 042 #3): OpenTelemetry's ten seconds, its retries inside them, is long for
// a request that flushes before it answers.
const defaultExportTimeout = 5 * time.Second

// exportTimeout is the option, then TRACEPAD_EXPORT_TIMEOUT in seconds, then
// five seconds — or zero, which leaves the exporter to OpenTelemetry's own
// variable when one is set.
func exportTimeout(given time.Duration) time.Duration {
	if given > 0 {
		return given
	}
	if given < 0 {
		def.log().Warn("tracepad.Init: WithExportTimeout is not a positive duration; it is ignored", "value", given)
	}
	if raw := strings.TrimSpace(os.Getenv("TRACEPAD_EXPORT_TIMEOUT")); raw != "" {
		seconds, err := strconv.ParseFloat(raw, 64)
		// Past the int64 of a Duration, or under a nanosecond — which would be
		// zero, and zero leaves the exporter to OpenTelemetry — is no timeout.
		if timeout := time.Duration(seconds * float64(time.Second)); err == nil && timeout > 0 && seconds < math.MaxInt64/float64(time.Second) {
			return timeout
		}
		def.log().Warn("tracepad.Init: TRACEPAD_EXPORT_TIMEOUT is not a number of seconds; it is ignored", "value", raw)
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TIMEOUT") != "" {
		return 0
	}
	return defaultExportTimeout
}

// isDefault tells the API's own delegating provider — the one every process
// has until something sets one, or what reset leaves — from a provider an
// application set that is not the SDK's. There is no public way to ask, and replacing an
// application's provider because its type was unfamiliar is exactly what
// Decision 2 refuses, so the package's own internal path is the test.
func isDefault(provider trace.TracerProvider) bool {
	if isFollow(provider) {
		return true
	}
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
	provider := otel.GetTracerProvider()
	if explicit := def.provider.Load(); explicit != nil {
		provider = *explicit
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
// provider is the application's to close. Every step keeps to the context's
// deadline: a store that is away at shutdown must not hold the process
// until something kills it.
func (d *defaults) close(ctx context.Context) error {
	err := Flush(ctx)
	d.mu.Lock()
	scores, sdk, built := d.scores, d.sdk, d.built
	d.mu.Unlock()
	if scores != nil {
		err = errors.Join(err, scores.close(ctx))
	}
	if built && sdk != nil {
		err = errors.Join(err, sdk.Shutdown(ctx))
	}
	return err
}

func init() {
	hook.Reset = reset
	hook.Capture = func(host, key string, provider trace.TracerProvider, keep func(map[string]any)) error {
		reset()
		followed.Store(&provider)
		q := newScoreQueue(nil)
		q.keep = keep
		d := def
		d.mu.Lock()
		defer d.mu.Unlock()
		d.scores = q
		// The propagator an Init that built its provider would set.
		if len(otel.GetTextMapPropagator().Fields()) == 0 {
			otel.SetTextMapPropagator(propagation.TraceContext{})
			d.propagated = true
		}
		_, err := d.start(context.Background(), config{host: host, key: key}, options{provider: provider})
		return err
	}
}

// follow is the global provider reset leaves (spec 040 #9, #14). The API's
// own default cannot come back once anything was set, and the first
// SetTracerProvider binds every tracer taken before it — a package-level
// otel.Tracer — to that provider for good. Bound to this one, such a tracer
// asks at every span where spans go now: to the capture's provider, to one
// set since, or nowhere — the no-op that is tracing off, and nothing set to
// an Init that runs next.
type follow struct{ embedded.TracerProvider }

// followed is the provider of the capture in progress.
var followed atomic.Pointer[trace.TracerProvider]

func (follow) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return followTracer{name: name, opts: opts}
}

type followTracer struct {
	embedded.Tracer
	name string
	opts []trace.TracerOption
}

var nowhere trace.TracerProvider = noop.NewTracerProvider()

func (t followTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	provider := nowhere
	if p := followed.Load(); p != nil {
		provider = *p
	} else if global := otel.GetTracerProvider(); !isFollow(global) {
		provider = global
	}
	return provider.Tracer(t.name, t.opts...).Start(ctx, name, opts...)
}

func isFollow(provider trace.TracerProvider) bool {
	_, ok := provider.(follow)
	return ok
}

// reset returns the process to never initialised — for this package's tests
// and, through the hook, for tracepadtest's (spec 040 #3). Init is
// process-wide by design. What Init built is shut down and what it set is
// taken back; the queue drops what it holds, outside the lock: its goroutine
// reads the configuration under it.
func reset() {
	followed.Store(nil)
	otel.SetTracerProvider(follow{})
	d := def
	d.mu.Lock()
	scores, sdk, built, propagated := d.scores, d.sdk, d.built, d.propagated
	d.built, d.propagated = false, false
	d.ready.Store(false)
	d.config, d.sdk, d.scores, d.shutdown = nil, nil, nil, nil
	d.logger.Store(nil)
	d.provider.Store(nil)
	d.mu.Unlock()
	if scores != nil {
		scores.drop()
	}
	if built {
		// Its spans go to the store the test configured, as at exit; a store
		// that is away does not hold the cleanup for the exporter's retries.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = sdk.Shutdown(ctx)
		cancel()
	}
	if propagated {
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	}
	forgetPrompts()
	warnedKinds.Lock()
	clear(warnedKinds.seen)
	warnedKinds.Unlock()
	hostWarned.Store(false)
}
