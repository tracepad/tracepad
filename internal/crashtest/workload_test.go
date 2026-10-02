package crashtest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// The ledger is what the clients were told. Everything the test asserts about
// durability comes from here: an answer the server gave before it was killed
// is a promise, and a request it never answered is only a possibility.
type ledger struct {
	mu sync.Mutex

	// traces holds every acknowledged trace of `main`: the spans a 2xx was
	// received for, and whose they are.
	traces map[string]*ackedTrace
	// open are the traces nobody is using at the moment; a worker takes one
	// out of the pool for the length of its request, so that a delete never
	// races a late span or a score aimed at the same trace.
	open []string
	// scores are the acknowledged scores and the trace each belongs to.
	scores map[string]string
	// deleteSent are the traces a DELETE was sent for, and deleteAcked those
	// it was answered for.
	deleteSent  map[string]bool
	deleteAcked map[string]bool
	// users: which user is being erased, how many requests are in flight for
	// each (an erasure waits for them), and what the erasure's answers were.
	claimed     map[string]bool
	inflight    map[string]int
	erasureSent map[string]bool
	erasureID   map[string]string // user -> id from a 202 or 200
	eraseUsers  []string          // erase-pool users not yet claimed
}

type ackedTrace struct {
	user  string
	spans map[string]bool
}

func newLedger() *ledger {
	return &ledger{
		traces:      map[string]*ackedTrace{},
		scores:      map[string]string{},
		deleteSent:  map[string]bool{},
		deleteAcked: map[string]bool{},
		claimed:     map[string]bool{},
		inflight:    map[string]int{},
		erasureSent: map[string]bool{},
		erasureID:   map[string]string{},
	}
}

// begin registers a request for a user, refusing when the user is already
// claimed by an erasure: nothing may be written for a user whose data is
// going, or the check afterwards would have no meaning.
func (l *ledger) begin(user string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claimed[user] {
		return false
	}
	l.inflight[user]++
	return true
}

func (l *ledger) end(user string) {
	l.mu.Lock()
	l.inflight[user]--
	l.mu.Unlock()
}

// workload is the traffic of one server process.
type workload struct {
	host string
	led  *ledger
	rng  *rand.Rand
	rmu  sync.Mutex

	client *http.Client
	// requests counts what was answered 2xx, for the log.
	acked atomic.Int64
	sent  atomic.Int64

	round      int
	regular    []string
	projectIDs map[string]string // name -> id
}

func (w *workload) intn(n int) int {
	w.rmu.Lock()
	defer w.rmu.Unlock()
	return w.rng.Intn(n)
}

func (w *workload) hexID(bytesLen int) string {
	w.rmu.Lock()
	defer w.rmu.Unlock()
	b := make([]byte, bytesLen)
	w.rng.Read(b)
	return hex.EncodeToString(b)
}

// pause is a short random wait, so that the workers neither spin nor line up.
func (w *workload) pause(ctx context.Context, max int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(w.intn(max)) * time.Millisecond):
		return true
	}
}

// run starts every worker and returns a function that waits for them. They
// stop when ctx is cancelled, or when the server stops answering.
func (w *workload) run(ctx context.Context) (wait func()) {
	var wg sync.WaitGroup
	start := func(name string, fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx)
		}()
	}
	// Both OTLP doors: the native path with a bearer key, and the path the
	// Langfuse SDK posts to with basic credentials.
	start("otlp-1", func(ctx context.Context) { w.ingestLoop(ctx, otlpBearer) })
	start("otlp-2", func(ctx context.Context) { w.ingestLoop(ctx, otlpBearer) })
	start("otlp-3", func(ctx context.Context) { w.ingestLoop(ctx, otlpBearer) })
	start("langfuse", func(ctx context.Context) { w.ingestLoop(ctx, otlpLangfuse) })
	start("langfuse-2", func(ctx context.Context) { w.ingestLoop(ctx, otlpLangfuse) })
	start("retained-1", w.retainedLoop)
	start("retained-2", w.retainedLoop)
	start("scores", w.scoreLoop)
	start("delete", w.deleteLoop)
	start("erase", w.eraseLoop)
	start("read", w.readLoop)
	return wg.Wait
}

type door int

const (
	otlpBearer door = iota
	otlpLangfuse
)

// call sends a request and returns the status, or -1 when there was no
// answer. A server killed mid-request is the normal end of every worker.
func (w *workload) call(ctx context.Context, method, path, auth string, body []byte, contentType string) (int, []byte) {
	req, err := http.NewRequestWithContext(ctx, method, w.host+path, bytes.NewReader(body))
	if err != nil {
		return -1, nil
	}
	req.Header.Set("Authorization", auth)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w.sent.Add(1)
	resp, err := w.client.Do(req)
	if err != nil {
		return -1, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		// The status line came, the body did not: the server died after
		// it had answered. The answer still counts.
		return resp.StatusCode, nil
	}
	return resp.StatusCode, data
}

func bearer(secret string) string { return "Bearer " + secret }

func basic(public, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(public+":"+secret))
}

// --- OTLP --------------------------------------------------------------

func strAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func intAttr(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_IntValue{IntValue: value}}}
}

// spanSpec is one span of an export, the way the test wants it written.
type spanSpec struct {
	id, parent string
	name       string
	failed     bool
	model      string
	payload    string
}

// exportBody encodes one export: the spans of a single trace.
func exportBody(traceID, user string, startNanos int64, specs []spanSpec) []byte {
	traceBytes, _ := hex.DecodeString(traceID)
	spans := make([]*tracepb.Span, 0, len(specs))
	for i, sp := range specs {
		id, _ := hex.DecodeString(sp.id)
		start := startNanos + int64(i)*int64(time.Millisecond)
		span := &tracepb.Span{
			TraceId:           traceBytes,
			SpanId:            id,
			Name:              sp.name,
			Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
			StartTimeUnixNano: uint64(start),
			EndTimeUnixNano:   uint64(start + 40*int64(time.Millisecond)),
			Attributes: []*commonpb.KeyValue{
				strAttr("langfuse.observation.input", sp.payload),
				strAttr("langfuse.observation.output", "out "+sp.payload),
			},
		}
		if sp.parent != "" {
			parent, _ := hex.DecodeString(sp.parent)
			span.ParentSpanId = parent
		}
		if user != "" {
			span.Attributes = append(span.Attributes, strAttr("langfuse.user.id", user))
		}
		if sp.model != "" {
			span.Attributes = append(span.Attributes,
				strAttr("langfuse.observation.model.name", sp.model),
				intAttr("gen_ai.usage.input_tokens", 11), intAttr("gen_ai.usage.output_tokens", 7))
		}
		if sp.failed {
			span.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "boom"}
			span.Attributes = append(span.Attributes, strAttr("langfuse.observation.level", "ERROR"))
		}
		spans = append(spans, span)
	}
	// TracesData and ExportTraceServiceRequest are one message on the wire:
	// the request is the resource spans and nothing else.
	req := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}}
	body, err := proto.Marshal(req)
	if err != nil {
		panic(err)
	}
	return body
}

var words = []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}

func (w *workload) specs(n int, rootID string) []spanSpec {
	out := make([]spanSpec, n)
	for i := range out {
		id := w.hexID(8)
		if i == 0 && rootID != "" {
			id = rootID
		}
		out[i] = spanSpec{
			id:      id,
			name:    "step-" + words[w.intn(len(words))],
			failed:  w.intn(5) == 0,
			payload: fmt.Sprintf("%s %s %d", words[w.intn(len(words))], words[w.intn(len(words))], w.intn(1000)),
		}
		if i > 0 {
			out[i].parent = out[0].id
		}
		if w.intn(3) == 0 {
			out[i].model = "model-" + words[w.intn(3)]
		}
	}
	return out
}

func (w *workload) postExport(ctx context.Context, d door, secretPublic [2]string, body []byte) int {
	path, auth := "/v1/traces", bearer(secretPublic[1])
	if d == otlpLangfuse {
		path, auth = "/api/public/otel/v1/traces", basic(secretPublic[0], secretPublic[1])
	}
	status, _ := w.call(ctx, "POST", path, auth, body, "application/x-protobuf")
	return status
}

// ingestLoop writes traces into `main`: a new trace most of the time, more
// spans of one already acknowledged a third of it, and now and then the same
// body again — an exporter's retry. Hours are spread over the last five, so
// that the aggregator has closed hours to roll and late spans to re-roll.
func (w *workload) ingestLoop(ctx context.Context, d door) {
	keys := [2]string{mainPublic, mainSecret}
	for w.pause(ctx, 6) {
		switch roll := w.intn(10); {
		case roll < 7:
			w.newTrace(ctx, d, keys)
		case roll < 9:
			w.lateSpans(ctx, d, keys)
		default:
			w.newTrace(ctx, d, keys) // and its retry, inside
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// pickUser is a user of the regular pool, or one of the erase pool while it
// still has members.
func (w *workload) pickUser() string {
	w.led.mu.Lock()
	defer w.led.mu.Unlock()
	if len(w.led.eraseUsers) > 0 && w.intn(3) == 0 {
		return w.led.eraseUsers[w.intn(len(w.led.eraseUsers))]
	}
	return w.regular[w.intn(len(w.regular))]
}

func (w *workload) newTrace(ctx context.Context, d door, keys [2]string) {
	user := w.pickUser()
	if !w.led.begin(user) {
		return
	}
	defer w.led.end(user)
	traceID := w.hexID(16)
	// Now and then an export too large for one slice (spec 043 #11): it is
	// stored in several transactions, so a kill can leave a prefix of it,
	// each half consistent in itself — and only its answer is a promise.
	n := 1 + w.intn(5)
	if w.intn(40) == 0 {
		n = 1100 + w.intn(600)
	}
	specs := w.specs(n, "")
	hoursAgo := time.Duration(w.intn(5*60)) * time.Minute
	start := time.Now().Add(-hoursAgo).UnixNano()
	body := exportBody(traceID, user, start, specs)
	status := w.postExport(ctx, d, keys, body)
	if status/100 != 2 {
		return
	}
	w.acked.Add(1)
	w.led.mu.Lock()
	rec := &ackedTrace{user: user, spans: map[string]bool{}}
	for _, sp := range specs {
		rec.spans[sp.id] = true
	}
	w.led.traces[traceID] = rec
	w.led.open = append(w.led.open, traceID)
	w.led.mu.Unlock()
	if w.intn(5) == 0 { // the exporter's retry of the same body
		w.postExport(ctx, d, keys, body)
	}
}

// lease takes a trace out of the open pool, for a request that must not race
// another request about the same trace.
func (w *workload) lease(allowErase bool) (id string, user string, ok bool) {
	w.led.mu.Lock()
	defer w.led.mu.Unlock()
	for tries := 0; tries < 8 && len(w.led.open) > 0; tries++ {
		i := w.intn(len(w.led.open))
		id = w.led.open[i]
		rec := w.led.traces[id]
		if w.led.claimed[rec.user] {
			continue
		}
		if !allowErase && isEraseUser(rec.user) {
			continue
		}
		w.led.open = append(w.led.open[:i], w.led.open[i+1:]...)
		w.led.inflight[rec.user]++
		return id, rec.user, true
	}
	return "", "", false
}

func (w *workload) release(id, user string, back bool) {
	w.led.mu.Lock()
	w.led.inflight[user]--
	if back {
		w.led.open = append(w.led.open, id)
	}
	w.led.mu.Unlock()
}

func (w *workload) lateSpans(ctx context.Context, d door, keys [2]string) {
	id, user, ok := w.lease(true)
	if !ok {
		return
	}
	specs := w.specs(1+w.intn(3), "")
	for i := range specs {
		specs[i].parent = ""
	}
	// The late spans carry the trace's user too: a span that named another
	// would change it, and the check asserts the user is the one first seen.
	status := w.postExport(ctx, d, keys, exportBody(id, user, time.Now().UnixNano(), specs))
	if status/100 == 2 {
		w.acked.Add(1)
		w.led.mu.Lock()
		for _, sp := range specs {
			w.led.traces[id].spans[sp.id] = true
		}
		w.led.mu.Unlock()
	}
	w.release(id, user, true)
}

// retainedLoop writes into `ret`, whose rows the test backdates past its
// retention window between processes: the backlog the sweeper is killed in
// the middle of. Its traces are wide — forty spans and more, each with
// its search entries — so that deleting a chunk of them is a transaction
// long enough for a kill to land in.
func (w *workload) retainedLoop(ctx context.Context) {
	keys := [2]string{retPublic, retSecret}
	for w.pause(ctx, 20) {
		traceID := w.hexID(16)
		body := exportBody(traceID, "r", time.Now().Add(-9*24*time.Hour).UnixNano(), w.specs(40+w.intn(60), ""))
		w.postExport(ctx, otlpBearer, keys, body)
	}
}

// scoreLoop scores traces. A score carries its own id, so a retry is the
// same score.
func (w *workload) scoreLoop(ctx context.Context) {
	for w.pause(ctx, 15) {
		id, user, ok := w.lease(true)
		if !ok {
			continue
		}
		scoreID := w.hexID(16)
		body, _ := json.Marshal(map[string]any{
			"id": scoreID, "trace_id": id, "name": "quality", "value": float64(w.intn(100)) / 100,
		})
		status, _ := w.call(ctx, "POST", "/api/v1/scores", bearer(mainSecret), body, "application/json")
		if status/100 == 2 {
			w.led.mu.Lock()
			w.led.scores[scoreID] = id
			w.led.mu.Unlock()
		}
		w.release(id, user, true)
	}
}

// deleteLoop deletes traces one at a time, the confirmed way. A trace it
// takes is out of the pool for good.
func (w *workload) deleteLoop(ctx context.Context) {
	for w.pause(ctx, 120) {
		id, user, ok := w.lease(false)
		if !ok {
			continue
		}
		w.led.mu.Lock()
		w.led.deleteSent[id] = true
		w.led.mu.Unlock()
		status, _ := w.call(ctx, "DELETE", "/api/v1/traces/"+id+"?confirm="+id, bearer(mainSecret), nil, "")
		if status/100 == 2 {
			w.led.mu.Lock()
			w.led.deleteAcked[id] = true
			w.led.mu.Unlock()
		}
		w.release(id, user, false)
	}
}

func isEraseUser(user string) bool { return len(user) > 2 && user[:2] == "e-" }

// eraseLoop claims one user of the erase pool at a time, waits for the
// requests in flight for it, and asks for the erasure. The user's traffic
// stops for good the moment it is claimed.
func (w *workload) eraseLoop(ctx context.Context) {
	for w.pause(ctx, 400) {
		w.led.mu.Lock()
		var user string
		for _, candidate := range w.led.eraseUsers {
			if w.countFor(candidate) >= 3 {
				user = candidate
				break
			}
		}
		if user != "" {
			w.led.claimed[user] = true
			rest := w.led.eraseUsers[:0]
			for _, u := range w.led.eraseUsers {
				if u != user {
					rest = append(rest, u)
				}
			}
			w.led.eraseUsers = rest
		}
		w.led.mu.Unlock()
		if user == "" {
			continue
		}
		for {
			w.led.mu.Lock()
			busy := w.led.inflight[user]
			w.led.mu.Unlock()
			if busy == 0 {
				break
			}
			if !w.pause(ctx, 10) {
				return
			}
		}
		w.led.mu.Lock()
		w.led.erasureSent[user] = true
		w.led.mu.Unlock()
		status, data := w.call(ctx, "DELETE",
			"/api/v1/projects/"+w.projectIDs["main"]+"/users/"+user+"/data?confirm="+user,
			bearer(mainSecret), nil, "")
		if status == http.StatusAccepted || status == http.StatusOK {
			var answer struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(data, &answer) == nil && answer.ID != "" {
				w.led.mu.Lock()
				w.led.erasureID[user] = answer.ID
				w.led.mu.Unlock()
			}
		}
	}
}

// countFor counts a user's acknowledged traces; the caller holds the lock.
func (w *workload) countFor(user string) int {
	n := 0
	for _, rec := range w.led.traces {
		if rec.user == user {
			n++
		}
	}
	return n
}

// readLoop is the export and the readers: the raw archive paged oldest
// first, the listing, the statistics, the system gauges. Their answers do not
// matter; they are there to hold read snapshots open across the kill.
func (w *workload) readLoop(ctx context.Context) {
	paths := []string{
		"/api/v1/raw?limit=20", "/api/v1/traces?limit=50", "/api/v1/stats?group_by=hour",
		"/api/v1/system", "/api/v1/sessions", "/api/v1/users",
	}
	for w.pause(ctx, 25) {
		w.call(ctx, "GET", paths[w.intn(len(paths))], bearer(mainSecret), nil, "")
	}
}
