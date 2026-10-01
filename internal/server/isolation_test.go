package server

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
)

/*
The isolation matrix (spec 028 #34, spec 004 Decision 33).

TestPermissionMatrix asks who may call a route. This asks what a caller of
project A meets when it names project B's rows: a key of A, and an editor and a
viewer whose only role is in A. The answer must be the answer A gets for a row
that exists nowhere — the same status and, once the identifiers are taken out,
the same body — so that nothing about B, not even that its row exists, crosses.
On a route that addresses one row that answer is `404`, reads and writes
alike. A route under `/api/v1/projects/{id}` naming B's project is the refusal
spec 028 Decision 6 gives every project the caller is not in, which is the same
`403` whether the project exists or not.

Three tenants: A, which the callers belong to; B, which holds a row of every
kind a route can name; and the ghost, which holds nothing and whose identifiers
have the same shape as B's. Every route is called with B's identifiers and with
the ghost's, and the two answers are compared — this is what turns "no leak of
existence" into a check rather than a reading of status codes.

`isolationMatrix` has one row per route in the table, which is the router and,
by TestOpenAPIMatchesRouter, the OpenAPI document. A route without a row fails
the test, and so does a row without a route: a new route is classified here
before it is trusted (spec 028 #34).

At the end B's rows are compared, column by column, with what they were before
any of it: every table that carries a project id, and the project row itself.
Then B exports and deletes, and every listing A reads must answer as it did
before: a number of A's that moves with B's traffic is a leak no identifier
crosses (spec 028 #35, spec 004 #37).
*/

// isolationKind is what a route does with the identifiers it is given.
type isolationKind int

const (
	// aboutPeople is a route about accounts, sign-in or the process rather
	// than a project's rows; the permission matrix is the whole of it.
	aboutPeople isolationKind = iota + 1
	// oneRow addresses one row of the caller's project by the values in its
	// path: a foreign value is a 404, whatever the method.
	oneRow
	// projectScoped is a route under `/api/v1/projects/{id}`: B's project id
	// is Decision 6's refusal, and a further path value of B's under A's
	// project is oneRow's 404, or an action when `action` is set.
	projectScoped
	// listing reads the caller's project with no path value; its filters are
	// probed with B's identifiers.
	listing
	// createsByName makes the caller's own row of the name in its path on
	// first write, so a foreign name is A's to take; B's row stays as it was.
	createsByName
	// write is a write with no path value, into the caller's project; what
	// it may name of B's is in its body or its query.
	write
)

// isolationRow is a route's line in the matrix.
type isolationRow struct {
	kind isolationKind
	// why is what makes an aboutPeople route nobody's project, or why none
	// of A's three callers may call a route that is a project's.
	why string
	// query is sent on every call; `{field}` names one of the variant
	// tenant's identifiers.
	query string
	// body is what a write sends, built from the tenant whose identifiers
	// the call names and the caller's own.
	body func(foreign, own *tenant) any
	// probes are queries a listing is called with, each naming one of the
	// foreign tenant's identifiers, so that a filter that reaches past the
	// project is caught on its own rather than masked by another.
	probes []string
	// refs are calls made with A's own path values and B's identifiers in
	// the body or the query, after every row has been probed: they may
	// create A's rows under B's ids, and must change nothing of B's.
	refs []isolationRef
	// action marks a projectScoped route whose second path value is an
	// argument rather than a row: it acts on A's project, and is compared
	// with the ghost after the rows have been probed.
	action bool
	// status is a oneRow route's answer to a foreign value, when it is not 404,
	// and a listing's to a filter that matches nothing, when it is not 200.
	status int
}

// isolationRef is one call of a route with A's own path values.
type isolationRef struct {
	query string
	body  func(foreign, own *tenant) any
	// carries is what both answers must contain, as on the row.
	carries string
}

// tenant is one project's identifiers, by the names a route takes. The ghost
// is a tenant that was never seeded.
type tenant struct {
	n           int
	tag, secret string
	projectID   string
	// content is in what its rows hold, where nothing of another tenant's
	// can echo it.
	content                                  string
	trace, span, session, user, gone         string
	score, prompt, dataset, item, run, other string
	config, queue, queueItem                 string
	raw, erasure, publicKey, sha, mediaID    string
	picture                                  []byte
}

func newTenant(tag, secret, projectID, publicKey string, n int) *tenant {
	picture := testPicture(5000+n, byte(n))
	sha := hexSHA(picture)
	return &tenant{
		n: n, tag: tag, secret: secret, projectID: projectID, publicKey: publicKey,
		content: "CONTENT-OF-" + strings.ToUpper(tag),
		trace:   traceHex(1000 + n), span: spanHex(1000 + n),
		session: tag + "-session", user: tag + "-user", gone: tag + "-gone",
		prompt: tag + "-prompt", dataset: tag + "-dataset", config: tag + "-config", queue: tag + "-queue",
		item: itemHex(n), run: runHex(n), other: runHex(n + 50),
		picture: picture, sha: sha, mediaID: store.MediaIDFor(sha),
		// What the ghost names and nothing holds; a seeded tenant's
		// are overwritten by what the server gives it.
		score: fmt.Sprintf("%032x", 0xf000+n), queueItem: fmt.Sprintf("%032x", 0xf100+n),
		erasure: fmt.Sprintf("%032x", 0xf200+n), raw: strconv.Itoa(9999990 + n),
	}
}

// fields are a tenant's identifiers by the `{name}` a template uses.
func (tn *tenant) fields() map[string]string {
	return map[string]string{
		"project": tn.projectID, "trace": tn.trace, "span": tn.span, "session": tn.session,
		"user": tn.user, "score": tn.score, "prompt": tn.prompt, "dataset": tn.dataset,
		"item": tn.item, "run": tn.run, "other": tn.other, "config": tn.config, "queue": tn.queue,
		"queueItem": tn.queueItem, "raw": tn.raw, "erasure": tn.erasure, "publicKey": tn.publicKey,
		"sha": tn.sha, "mediaID": tn.mediaID, "content": tn.content,
	}
}

// expand fills a template's `{field}`s from the tenant.
func (tn *tenant) expand(template string) string {
	for name, value := range tn.fields() {
		template = strings.ReplaceAll(template, "{"+name+"}", url.QueryEscape(value))
	}
	return template
}

// markers are the identifiers that must never appear in an answer to A, unless
// the request itself carried them. Short or shared ones — a raw batch's
// integer, a label — are not markers.
func (tn *tenant) markers() []string {
	return []string{tn.projectID, tn.content, tn.trace, tn.span, tn.session, tn.user,
		tn.score, tn.item, tn.run, tn.other, tn.queueItem, tn.erasure, tn.publicKey, tn.sha, tn.mediaID}
}

// param is what a route's path value means, by the route: the tenant's row of
// the kind the route is about. ok is false for a value the matrix cannot
// fill, which fails the test until it is taught.
func (tn *tenant) param(pattern, name string) (string, bool) {
	rest := strings.TrimPrefix(pattern, "/api/v1/")
	first, _, _ := strings.Cut(rest, "/")
	switch name {
	case "name":
		value, ok := map[string]string{"prompts": tn.prompt, "datasets": tn.dataset,
			"score-configs": tn.config, "queues": tn.queue}[first]
		return value, ok
	case "id":
		switch {
		case first == "datasets":
			return tn.item, true
		case first == "queues":
			return tn.queueItem, true
		}
		value, ok := map[string]string{"traces": tn.trace, "observations": tn.span,
			"sessions": tn.session, "users": tn.user, "scores": tn.score, "runs": tn.run,
			"raw": tn.raw, "projects": tn.projectID}[first]
		return value, ok
	case "a":
		return tn.run, true
	case "b":
		return tn.other, true
	case "user_id":
		return tn.user, true
	case "erasure_id":
		return tn.erasure, true
	case "public_key":
		return tn.publicKey, true
	case "sha256":
		return tn.sha, true
	case "mediaId":
		return tn.mediaID, true
	}
	return "", false
}

// pathParams are a pattern's path values in order, without `{label}`, which
// names a label rather than anybody's row.
func pathParams(pattern string) []string {
	var out []string
	for _, part := range strings.Split(pattern, "/") {
		if strings.HasPrefix(part, "{") && part != "{label}" {
			out = append(out, strings.Trim(part, "{}"))
		}
	}
	return out
}

// pathWith fills a pattern, taking each value from the tenant `from` names.
func pathWith(pattern string, from map[string]*tenant) (string, bool) {
	path := strings.ReplaceAll(pattern, "{label}", "production")
	for _, name := range pathParams(pattern) {
		value, ok := from[name].param(pattern, name)
		if !ok {
			return "", false
		}
		path = strings.Replace(path, "{"+name+"}", url.PathEscape(value), 1)
	}
	return path, true
}

func fixedBody(v any) func(foreign, own *tenant) any {
	return func(*tenant, *tenant) any { return v }
}

// scoreBody is a score on whatever the fields name.
func scoreBody(fields map[string]any) map[string]any {
	body := map[string]any{"name": "isolation-probe", "value": 1}
	for key, value := range fields {
		body[key] = value
	}
	return body
}

// isolationMatrix is the matrix: one row per route.
var isolationMatrix = map[string]isolationRow{
	"GET /health": {kind: aboutPeople, why: "liveness; names no project"},
	"POST /v1/traces": {kind: write, refs: []isolationRef{
		{body: func(f, own *tenant) any { return ingestExport(f, "isolation-probe", nil) }}}},
	"POST /api/public/otel/v1/traces": {kind: write, refs: []isolationRef{
		{body: func(f, own *tenant) any { return ingestExport(f, "isolation-probe", nil) }}}},
	// Asking for an upload of B's body is asked for the bytes, because the
	// bytes are the proof of possession (spec 041 #9).
	// A's own body on B's trace id is A's ref on a trace A does not have yet:
	// nothing asked for, as for any trace.
	"POST /api/public/media": {kind: write, refs: []isolationRef{
		{body: func(f, own *tenant) any { return mediaAsk(f.picture, own.trace) }, carries: `"uploadUrl":"http`},
		{body: func(f, own *tenant) any { return mediaAsk(own.picture, f.trace) }, carries: `"uploadUrl":null`}}},
	// The presigned PUT is the token's, not a credential's: A's grant names
	// A's body, and spent on B's media id it is void.
	"PUT /api/public/media/{mediaId}/upload": {kind: oneRow, status: http.StatusForbidden},
	// The report on an upload is logged and answered 204 whoever asks
	// (spec 041 #9), so it is the same answer for every id.
	"PATCH /api/public/media/{mediaId}": {kind: oneRow, status: http.StatusNoContent,
		body: fixedBody(map[string]any{"uploadedAt": "2026-01-01T00:00:00Z", "uploadHttpStatus": 200})},
	"GET /api/public/media/{mediaId}": {kind: oneRow},

	"GET /api/v1":              {kind: aboutPeople, why: "the endpoint map; names no project"},
	"GET /api/v1/openapi.json": {kind: aboutPeople, why: "the API's description; names no project"},
	"GET /api/v1/system":       {kind: listing},

	"GET /api/v1/setup":               {kind: aboutPeople, why: "the first owner; before any project is anyone's"},
	"POST /api/v1/setup":              {kind: aboutPeople, why: "the first owner; before any project is anyone's"},
	"POST /api/v1/auth/login":         {kind: aboutPeople, why: "a person signing in"},
	"POST /api/v1/auth/accept-invite": {kind: aboutPeople, why: "a person signing in"},
	"POST /api/v1/auth/logout":        {kind: aboutPeople, why: "the caller's own session"},
	"GET /api/v1/auth/me":             {kind: aboutPeople, why: "the caller's own account"},
	"PATCH /api/v1/auth/me":           {kind: aboutPeople, why: "the caller's own account"},
	"GET /api/v1/auth/sessions":       {kind: aboutPeople, why: "the caller's own sessions"},
	"DELETE /api/v1/auth/sessions":    {kind: aboutPeople, why: "the caller's own sessions"},

	"GET /api/v1/traces": {kind: listing, probes: []string{"user_id={user}", "session_id={session}",
		"run_id={run}", "item_id={item}", "q={content}", "name={content}+trace", "prompt={prompt}"}},
	// The newest match, or 404 when nothing of A's matches.
	"GET /api/v1/traces/last": {kind: listing, status: http.StatusNotFound, probes: []string{"user_id={user}", "session_id={session}",
		"run_id={run}", "item_id={item}", "q={content}"}},
	"GET /api/v1/traces/{id}":    {kind: oneRow},
	"DELETE /api/v1/traces/{id}": {kind: oneRow, query: "confirm={trace}"},
	// A bulk deletion is a listing filter over A's traces: the confirmed
	// round deletes A's rows the filter matches, and those are the rows the
	// earlier ingest made under B's ids, if any.
	"DELETE /api/v1/traces": {kind: write, refs: []isolationRef{
		{query: "user_id={user}&to=" + farFuture}, {query: "session_id={session}&to=" + farFuture},
		{query: "run_id={run}&to=" + farFuture}, {query: "user_id={user}&to=" + farFuture + "&confirm=test"}}},
	"GET /api/v1/observations/{id}/io": {kind: oneRow, refs: []isolationRef{{query: "trace_id={trace}"}}},
	"GET /api/v1/media/{sha256}":       {kind: oneRow},

	"GET /api/v1/raw":      {kind: listing},
	"GET /api/v1/raw/{id}": {kind: oneRow},

	"GET /api/v1/sessions":       {kind: listing, probes: []string{"user_id={user}"}},
	"GET /api/v1/sessions/{id}":  {kind: oneRow},
	"GET /api/v1/stats":          {kind: listing, probes: []string{"user_id={user}"}},
	"GET /api/v1/stats/scores":   {kind: listing, probes: []string{"name={content}-score"}},
	"GET /api/v1/facets":         {kind: listing},
	"GET /api/v1/users":          {kind: listing, probes: []string{"prefix={user}"}},
	"GET /api/v1/users/{id}":     {kind: oneRow},
	"GET /api/v1/scores/{id}":    {kind: oneRow},
	"DELETE /api/v1/scores/{id}": {kind: oneRow},
	"GET /api/v1/scores": {kind: listing, probes: []string{"trace_id={trace}", "observation_id={span}",
		"session_id={session}", "name={content}-score"}},
	// A score names its target by id and may carry its own: on A's side
	// each is A's, whatever B holds under the same string.
	"POST /api/v1/scores": {kind: write, refs: []isolationRef{
		{body: func(f, own *tenant) any { return scoreBody(map[string]any{"trace_id": f.trace}) }},
		{body: func(f, own *tenant) any {
			return scoreBody(map[string]any{"trace_id": f.trace, "observation_id": f.span})
		}},
		{body: func(f, own *tenant) any { return scoreBody(map[string]any{"session_id": f.session}) }},
		{body: func(f, own *tenant) any {
			return scoreBody(map[string]any{"id": f.score, "trace_id": own.trace})
		}}}},

	"GET /api/v1/prompts":                          {kind: listing},
	"GET /api/v1/prompts/{name}":                   {kind: oneRow, query: "label=production"},
	"POST /api/v1/prompts/{name}/versions":         {kind: createsByName, body: fixedBody(chatBody("isolation-probe", nil))},
	"GET /api/v1/prompts/{name}/versions":          {kind: oneRow},
	"GET /api/v1/prompts/{name}/diff":              {kind: oneRow, query: "from=1&to=2"},
	"DELETE /api/v1/prompts/{name}":                {kind: oneRow, query: "confirm={prompt}"},
	"PUT /api/v1/prompts/{name}/labels/{label}":    {kind: oneRow, body: fixedBody(map[string]any{"version": 1})},
	"DELETE /api/v1/prompts/{name}/labels/{label}": {kind: oneRow},

	"GET /api/v1/datasets":           {kind: listing},
	"PUT /api/v1/datasets/{name}":    {kind: createsByName, body: fixedBody(map[string]any{"description": "isolation-probe"})},
	"GET /api/v1/datasets/{name}":    {kind: oneRow},
	"DELETE /api/v1/datasets/{name}": {kind: oneRow, query: "confirm={dataset}"},
	"POST /api/v1/datasets/{name}/items": {kind: createsByName,
		body: fixedBody([]map[string]any{{"input": map[string]any{"x": 1}}}),
		refs: []isolationRef{{body: func(f, own *tenant) any {
			return []map[string]any{{"id": f.item, "input": map[string]any{"x": 1},
				"source_trace_id": f.trace, "source_observation_id": f.span}}
		}}}},
	"GET /api/v1/datasets/{name}/items":               {kind: oneRow},
	"GET /api/v1/datasets/{name}/items/{id}":          {kind: oneRow},
	"GET /api/v1/datasets/{name}/items/{id}/versions": {kind: oneRow},
	"DELETE /api/v1/datasets/{name}/items/{id}":       {kind: oneRow},
	"POST /api/v1/datasets/{name}/runs": {kind: oneRow, body: fixedBody(map[string]any{"id": runHex(77)}),
		refs: []isolationRef{{body: func(f, own *tenant) any { return map[string]any{"id": f.run} }}}},
	"GET /api/v1/datasets/{name}/runs": {kind: oneRow},
	"GET /api/v1/runs":                 {kind: listing, probes: []string{"dataset={dataset}"}},
	"GET /api/v1/runs/{id}":            {kind: oneRow},
	"GET /api/v1/runs/{id}/items":      {kind: oneRow},
	"GET /api/v1/runs/{a}/compare/{b}": {kind: oneRow},
	"POST /api/v1/runs/{id}/finish":    {kind: oneRow, body: fixedBody(map[string]any{"status": "finished"})},
	"DELETE /api/v1/runs/{id}":         {kind: oneRow},

	"GET /api/v1/score-configs": {kind: listing},
	"PUT /api/v1/score-configs/{name}": {kind: createsByName,
		body: fixedBody(map[string]any{"data_type": "numeric", "direction": "higher"})},
	"GET /api/v1/score-configs/{name}":    {kind: oneRow},
	"DELETE /api/v1/score-configs/{name}": {kind: oneRow},

	"GET /api/v1/queues": {kind: listing},
	// A queue names score configs by name, which are A's: B's config of the
	// same name is not one A's queue can ask for.
	"PUT /api/v1/queues/{name}": {kind: createsByName,
		body: func(f, own *tenant) any { return map[string]any{"score_configs": []string{f.config}} }},
	"GET /api/v1/queues/{name}":    {kind: oneRow},
	"DELETE /api/v1/queues/{name}": {kind: oneRow, query: "confirm={queue}"},
	"POST /api/v1/queues/{name}/items": {kind: oneRow,
		body: func(f, own *tenant) any { return map[string]any{"trace_id": f.trace} },
		refs: []isolationRef{
			{body: func(f, own *tenant) any { return map[string]any{"trace_id": f.trace} }},
			{body: func(f, own *tenant) any { return map[string]any{"trace_id": f.trace, "observation_id": f.span} }}}},
	"POST /api/v1/queues/{name}/items/from-traces": {kind: oneRow, refs: []isolationRef{
		{query: "user_id={user}"}, {query: "session_id={session}"}, {query: "run_id={run}"}}},
	"GET /api/v1/queues/{name}/items":                {kind: oneRow},
	"GET /api/v1/queues/{name}/next":                 {kind: oneRow, query: "annotator=ada"},
	"GET /api/v1/queues/{name}/items/{id}":           {kind: oneRow},
	"POST /api/v1/queues/{name}/items/{id}/complete": {kind: oneRow, body: fixedBody(map[string]any{"annotator": "ada"})},
	"POST /api/v1/queues/{name}/items/{id}/skip":     {kind: oneRow, body: fixedBody(map[string]any{"annotator": "ada"})},
	"POST /api/v1/queues/{name}/items/{id}/reopen":   {kind: oneRow, body: fixedBody(map[string]any{"annotator": "ada"})},
	"DELETE /api/v1/queues/{name}/items/{id}":        {kind: oneRow},

	"GET /api/v1/projects":                           {kind: listing},
	"POST /api/v1/projects":                          {kind: aboutPeople, why: "an owner creating a project; names none"},
	"GET /api/v1/projects/{id}":                      {kind: projectScoped},
	"PATCH /api/v1/projects/{id}":                    {kind: projectScoped, body: fixedBody(map[string]any{"retention_days": 7})},
	"DELETE /api/v1/projects/{id}":                   {kind: projectScoped, query: "confirm=other", why: ownersOnly},
	"POST /api/v1/projects/{id}/restore":             {kind: projectScoped, why: ownersOnly},
	"GET /api/v1/projects/{id}/keys":                 {kind: projectScoped},
	"POST /api/v1/projects/{id}/keys":                {kind: projectScoped, body: fixedBody(map[string]any{"scopes": []string{"read"}})},
	"DELETE /api/v1/projects/{id}/keys/{public_key}": {kind: projectScoped},
	// Erasing a user id is an act on A's rows of that id: B's user of the
	// same id is somebody else.
	"DELETE /api/v1/projects/{id}/users/{user_id}/data": {kind: projectScoped, action: true,
		query: "confirm={user}&wait=10"},
	"GET /api/v1/projects/{id}/erasures":              {kind: projectScoped},
	"GET /api/v1/projects/{id}/erasures/{erasure_id}": {kind: projectScoped},
	"GET /api/v1/projects/{id}/members":               {kind: projectScoped, why: ownersOnly},

	"GET /api/v1/accounts":                               {kind: aboutPeople, why: "the owner's accounts table"},
	"POST /api/v1/accounts":                              {kind: aboutPeople, why: "the owner's accounts table"},
	"GET /api/v1/accounts/{id}":                          {kind: aboutPeople, why: "the owner's accounts table"},
	"PATCH /api/v1/accounts/{id}":                        {kind: aboutPeople, why: "the owner's accounts table"},
	"DELETE /api/v1/accounts/{id}":                       {kind: aboutPeople, why: "the owner's accounts table"},
	"POST /api/v1/accounts/{id}/invite":                  {kind: aboutPeople, why: "the owner's accounts table"},
	"PUT /api/v1/accounts/{id}/projects/{project_id}":    {kind: aboutPeople, why: "an owner granting a role"},
	"DELETE /api/v1/accounts/{id}/projects/{project_id}": {kind: aboutPeople, why: "an owner taking a role away"},
}

// ingestExport is one span of the tenant's ids, as an exporter would send it:
// protobuf, so that the call is the OTLP route's own. A picture is the whole
// input, as a data URL, and is stored as a media body.
func ingestExport(tn *tenant, content string, picture []byte) []byte {
	input := content + " input"
	if picture != nil {
		input = "data:image/png;base64," + base64.StdEncoding.EncodeToString(picture)
	}
	span := otlptest.ProbeSpan(
		"langfuse.trace.name", content+" trace",
		"langfuse.user.id", tn.user,
		"langfuse.session.id", tn.session,
		"langfuse.observation.input", input,
		"langfuse.observation.output", content+" output",
	)
	span.TraceId, _ = hex.DecodeString(tn.trace)
	span.SpanId, _ = hex.DecodeString(tn.span)
	span.Name = content + " span"
	body, err := mapping.EncodeExportRequest(otlptest.Export(span))
	if err != nil {
		panic(err)
	}
	return body
}

// mediaAsk is the Langfuse SDK's request for an upload URL.
func mediaAsk(picture []byte, trace string) map[string]any {
	sum := sha256.Sum256(picture)
	return map[string]any{"traceId": trace, "observationId": spanHex(1), "contentType": "image/png",
		"contentLength": len(picture), "sha256Hash": base64.StdEncoding.EncodeToString(sum[:]), "field": "input"}
}

// pathVariants are the ways a path can name a foreign row: every value
// foreign, and, where there are two, each one alone under A's other.
func pathVariants(pattern string) []map[string]bool {
	params := pathParams(pattern)
	all := map[string]bool{}
	for _, p := range params {
		all[p] = true
	}
	out := []map[string]bool{all}
	if len(params) > 1 {
		for _, p := range params {
			out = append(out, map[string]bool{p: true})
		}
	}
	return out
}

// plannedPairs is how many paired calls — B's ids, then the ghost's — one
// caller makes on a route, by its row: what the walk below must have made.
func plannedPairs(rt route, row isolationRow) int {
	n := len(row.refs)
	switch row.kind {
	case oneRow:
		n += len(pathVariants(rt.Path))
	case projectScoped:
		for _, foreign := range pathVariants(rt.Path) {
			// An action's variants under A's own project are its
			// one call of phase two.
			if foreign["id"] || !row.action {
				n++
			}
		}
		if row.action {
			n++
		}
	case listing:
		n += len(row.probes)
	case createsByName:
		n++
	}
	if row.kind != aboutPeople && strings.HasPrefix(rt.Path, "/api/v1/") {
		n += 2 // ?project= and ?project_id=
	}
	return n
}

// isolationCaller is one of the three who belong to A alone.
type isolationCaller struct {
	who    who
	mutate []func(*http.Request)
}

func (c isolationCaller) isSession() bool { return c.who != projectKey }

// seedTenant gives a tenant a row of every kind a route can name, through the
// API with the tenant's own key, and reads back the identifiers the server
// chose.
func (h *harness) seedTenant(t *testing.T, tn *tenant) {
	t.Helper()
	as := asKey(tn.secret)
	post := func(method, path string, body any, want int) *httptest.ResponseRecorder {
		t.Helper()
		rec := h.call(t, method, path, mustJSON(t, body), as, asJSON)
		expectStatus(t, rec, want)
		return rec
	}

	// The trace arrives as spans do, so that it leaves a raw batch and a
	// held media body behind it.
	rec := h.post(t, "/v1/traces", ingestExport(tn, tn.content, tn.picture), as)
	expectStatus(t, rec, http.StatusOK)
	// A second user, whom the erasure below takes: the erasure is a row of
	// its own that a route reads by id.
	gone := *tn
	gone.trace, gone.span, gone.user = traceHex(2000+tn.n), spanHex(2000+tn.n), tn.gone
	expectStatus(t, h.post(t, "/v1/traces", ingestExport(&gone, tn.content, nil), as), http.StatusOK)

	raw := decodeJSON[rawListing](t, h.call(t, "GET", "/api/v1/raw", nil, as))
	if len(raw.Batches) == 0 {
		t.Fatalf("%s: no raw batch after ingest", tn.tag)
	}
	tn.raw = strconv.FormatInt(raw.Batches[0].ID, 10)

	rec = post("POST", "/api/v1/scores", map[string]any{"trace_id": tn.trace, "name": tn.content + "-score", "value": 1},
		http.StatusCreated)
	tn.score = decodeJSON[struct {
		IDs []string `json:"ids"`
	}](t, rec).IDs[0]

	post("POST", "/api/v1/prompts/"+tn.prompt+"/versions",
		chatBody(tn.content+" prompt text", map[string]any{"labels": []string{"production"}}), http.StatusCreated)
	post("POST", "/api/v1/prompts/"+tn.prompt+"/versions", chatBody(tn.content+" second", nil), http.StatusCreated)

	post("POST", "/api/v1/datasets/"+tn.dataset+"/items",
		[]map[string]any{{"id": tn.item, "input": map[string]any{"q": tn.content}}}, http.StatusCreated)
	post("POST", "/api/v1/datasets/"+tn.dataset+"/runs", map[string]any{"id": tn.run}, http.StatusCreated)
	post("POST", "/api/v1/datasets/"+tn.dataset+"/runs", map[string]any{"id": tn.other}, http.StatusCreated)

	post("PUT", "/api/v1/score-configs/"+tn.config, map[string]any{"data_type": "numeric", "direction": "higher"},
		http.StatusOK)
	post("PUT", "/api/v1/queues/"+tn.queue, map[string]any{"description": tn.content,
		"score_configs": []string{tn.config}}, http.StatusCreated)
	rec = post("POST", "/api/v1/queues/"+tn.queue+"/items", map[string]any{"trace_id": tn.trace}, http.StatusCreated)
	tn.queueItem = decodeJSON[struct {
		IDs []string `json:"ids"`
	}](t, rec).IDs[0]

	// The erasure is asked with the admin token: erasing is an editor's
	// and the tenant's key carries every scope, but the token is what an
	// operator would use, and it waits for the end.
	rec = h.call(t, "DELETE", "/api/v1/projects/"+tn.projectID+"/users/"+tn.gone+"/data?confirm="+tn.gone+"&wait=10",
		nil, asAdmin)
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("%s: erasure = %d (%s)", tn.tag, rec.Code, rec.Body)
	}
	tn.erasure = decodeJSON[erasureBody](t, rec).ID
	waitErasure(t, h, tn)

	if !h.mediaHeldBy(t, tn.projectID, tn.sha) {
		t.Fatalf("%s: the body its span carried is not held", tn.tag)
	}
}

// waitErasure waits for a tenant's erasure to end, so that nothing of B's
// moves under the fingerprint.
func waitErasure(t *testing.T, h *harness, tn *tenant) {
	t.Helper()
	path := "/api/v1/projects/" + tn.projectID + "/erasures/" + tn.erasure
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		rec := h.call(t, "GET", path, nil, asAdmin)
		expectStatus(t, rec, http.StatusOK)
		if decodeJSON[erasureBody](t, rec).State == store.ErasureDone {
			return
		}
	}
	t.Fatalf("%s: the erasure did not end", tn.tag)
}

// mediaHeldBy reports whether a project holds a body.
func (h *harness) mediaHeldBy(t *testing.T, projectID, sha string) bool {
	t.Helper()
	held, err := h.store.MediaHeld(t.Context(), projectID, sha)
	if err != nil {
		t.Fatal(err)
	}
	return held != nil
}

// relatedTables are the tables with no project id of their own, and how a
// project's rows in each are found: through the rows that point at them. `?1`
// is the project.
var relatedTables = map[string]string{
	"projects": `SELECT * FROM projects WHERE id = ?1`,
	// A payload is the body of a trace's metadata or an observation's
	// input, output or metadata, and belongs to the row that points at it.
	"payloads": `SELECT * FROM payloads WHERE id IN (
		SELECT metadata_id FROM traces WHERE project_id = ?1
		UNION SELECT input_id FROM observations WHERE project_id = ?1
		UNION SELECT output_id FROM observations WHERE project_id = ?1
		UNION SELECT metadata_id FROM observations WHERE project_id = ?1)`,
	// A body is shared by every project that holds it or points at it.
	"media": `SELECT * FROM media WHERE sha256 IN (
		SELECT sha256 FROM media_holders WHERE project_id = ?1
		UNION SELECT sha256 FROM media_refs WHERE project_id = ?1)`,
	"erasure_tail": `SELECT * FROM erasure_tail WHERE erasure_id IN (
		SELECT id FROM erasures WHERE project_id = ?1)`,
	// The people with a role in the project, and their ways in.
	"accounts": `SELECT * FROM accounts WHERE id IN (
		SELECT account_id FROM memberships WHERE project_id = ?1)`,
	"account_sessions": `SELECT * FROM account_sessions WHERE account_id IN (
		SELECT account_id FROM memberships WHERE project_id = ?1)`,
	"account_tokens": `SELECT * FROM account_tokens WHERE account_id IN (
		SELECT account_id FROM memberships WHERE project_id = ?1)`,
}

// unrelatedTables hold nothing of any one project, and say why. The full-text
// index is contentless — what it holds of a project is its `search_entries`
// rows, compared above, and B's own search at the end of the matrix.
var unrelatedTables = map[string]string{
	"schema_migrations":  "the migrations this file has applied",
	"compaction":         "the one row saying when the file was last compacted",
	"search_backfill":    "the one row saying whether the index was backfilled",
	"server_keys":        "the process's own signing keys",
	"search_fts":         "the contentless index over search_entries",
	"search_fts_data":    "the index's own storage",
	"search_fts_idx":     "the index's own storage",
	"search_fts_docsize": "the index's own storage",
	"search_fts_config":  "the index's own storage",
}

// fingerprint is every row of a project, column by column, in every table of
// the schema: by its project id where the table has one, through relatedTables
// where it does not. A table in neither, and not in unrelatedTables, fails the
// test: a new table says how it belongs to a project before it is trusted.
// Read from the file, not the API, so that a column no route shows is
// compared too.
func fingerprint(t *testing.T, h *harness, projectID string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tables, err := db.QueryContext(t.Context(), `
		SELECT m.name, EXISTS (SELECT 1 FROM pragma_table_info(m.name) WHERE name = 'project_id')
		  FROM sqlite_master m
		 WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'
		 ORDER BY m.name`)
	if err != nil {
		t.Fatal(err)
	}
	queries := map[string]string{}
	for tables.Next() {
		var name string
		var scoped bool
		if err := tables.Scan(&name, &scoped); err != nil {
			t.Fatal(err)
		}
		related, isRelated := relatedTables[name]
		_, isUnrelated := unrelatedTables[name]
		switch {
		case scoped && !isRelated && !isUnrelated:
			queries[name] = `SELECT * FROM "` + name + `" WHERE project_id = ?1`
		case !scoped && isRelated:
			queries[name] = related
		case !scoped && isUnrelated:
		default:
			t.Errorf("table %s (project id: %v) is not classified: give it a relation in relatedTables "+
				"or a reason in unrelatedTables", name, scoped)
		}
	}
	tables.Close()
	for name := range relatedTables {
		if _, ok := queries[name]; !ok {
			t.Errorf("relatedTables names %s, which the schema does not have, or which has a project id", name)
		}
	}
	out := map[string][]string{}
	for label, query := range queries {
		rows, err := db.QueryContext(t.Context(), query, projectID)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			var line strings.Builder
			for i, value := range values {
				if raw, ok := value.([]byte); ok {
					value = hex.EncodeToString(raw)
				}
				fmt.Fprintf(&line, "%s=%v;", columns[i], value)
			}
			out[label] = append(out[label], line.String())
		}
		rows.Close()
		sort.Strings(out[label])
	}
	return out
}

// pushRawNumbers moves a project's next raw batch number to seven digits, so
// that its batch's id is a token no count or size in an answer can be
// mistaken for, and the ghost's id has the same shape. A batch's number is its
// project's own (spec 019 #17), so this is B's counter alone; A's batches keep
// numbers of their own, far below it.
func pushRawNumbers(t *testing.T, h *harness, projectID string, last int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(),
		`UPDATE projects SET raw_batches_numbered = ? WHERE id = ?`, last, projectID); err != nil {
		t.Fatal(err)
	}
}

// normalizer replaces every identifier of the tenant in an answer by its
// field's name, so that B's answer and the ghost's compare equal exactly when
// they say the same thing. Whole words only, longest first, in one pass.
func (tn *tenant) normalizer() func(string) string {
	names := map[string]string{}
	for name, value := range tn.fields() {
		if value == "" {
			continue
		}
		names[value] = "<" + name + ">"
		names[url.QueryEscape(value)] = "<" + name + ">"
	}
	values := make([]string, 0, len(names))
	for value := range names {
		values = append(values, regexp.QuoteMeta(value))
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	pattern := regexp.MustCompile(`\b(?:` + strings.Join(values, "|") + `)\b`)
	return func(body string) string {
		return pattern.ReplaceAllStringFunc(body, func(match string) string { return names[match] })
	}
}

func TestIsolationMatrix(t *testing.T) {
	h := newAccountHarness(t)
	routes := h.server.routes()

	// Every route has a row, and every row a route.
	seen := map[string]bool{}
	for _, rt := range routes {
		key := rt.Method + " " + rt.Path
		seen[key] = true
		row, ok := isolationMatrix[key]
		switch {
		case !ok:
			t.Errorf("%s has no row in isolationMatrix: say what its path values and filters mean before it ships", key)
		case row.kind == aboutPeople && row.why == "":
			t.Errorf("%s is about people but does not say why", key)
		case row.kind != aboutPeople && rt.Policy == public:
			t.Errorf("%s is public, so it reaches no project's rows: it is aboutPeople", key)
		}
	}
	for key := range isolationMatrix {
		if !seen[key] {
			t.Errorf("isolationMatrix has a row for %s, which the table does not serve", key)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	a := newTenant("alpha", testSecret, h.project.ID, testPublic, 1)
	b := newTenant("bravo", bSecret, h.second(t, bProjectName, bSecret).ID, "tp-pk-"+bProjectName, 2)
	ghostID, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	ghost := newTenant("ghost", "tp-sk-nobody", ghostID, "tp-pk-nobody", 3)
	h.seedTenant(t, a)
	pushRawNumbers(t, h, b.projectID, 9999990+int64(b.n)-1)
	h.seedTenant(t, b)
	if len(b.raw) != len(ghost.raw) {
		t.Fatalf("B's raw batch is %s, the ghost's %s: the two must have one shape", b.raw, ghost.raw)
	}
	normalB, normalGhost := b.normalizer(), ghost.normalizer()
	before := fingerprint(t, h, b.projectID)
	// The fingerprint finds what the seeding made, through each relation as
	// well as by the project id: a comparison of empty sets would be no
	// comparison at all.
	for _, table := range []string{"projects", "api_keys", "traces", "observations", "payloads", "raw_batches",
		"media", "media_holders", "media_refs", "media_raw_refs", "search_entries", "scores", "prompts",
		"prompt_labels", "datasets", "dataset_items", "dataset_runs", "score_configs", "annotation_queues",
		"annotation_items", "erasures"} {
		if len(before[table]) == 0 {
			t.Errorf("the fingerprint holds no row of B's in %s", table)
		}
	}

	editor, viewer := h.editor(t), h.viewer(t)
	callers := []isolationCaller{
		{projectKey, nil},
		{editorSession, []func(*http.Request){asSession(editor), inProject(a.projectID)}},
		{viewerSession, []func(*http.Request){asSession(viewer), inProject(a.projectID)}},
	}

	// send makes one call as one caller. A signed-in reviewer names nobody
	// (spec 048 #15), so the annotator a key sends is dropped for a session.
	send := func(c isolationCaller, rt route, path, query string, payload []byte) *httptest.ResponseRecorder {
		if c.isSession() {
			query = strings.TrimPrefix(strings.ReplaceAll("&"+query, "&annotator=ada", ""), "&")
			if bytes.Contains(payload, []byte(`"annotator"`)) {
				payload = []byte(`{}`)
			}
		}
		if query != "" {
			path += "?" + query
		}
		mutate := append(slices.Clone(c.mutate), asJSON)
		if strings.HasSuffix(rt.Path, "/v1/traces") && rt.Method == "POST" {
			mutate = append(mutate, func(r *http.Request) { r.Header.Set("Content-Type", "application/x-protobuf") })
		}
		return h.call(t, rt.Method, path, payload, mutate...)
	}
	encode := func(body func(foreign, own *tenant) any, foreign *tenant) []byte {
		if body == nil {
			return []byte(`{}`)
		}
		value := body(foreign, a)
		if raw, ok := value.([]byte); ok {
			return raw
		}
		return mustJSONBytes(value)
	}
	// leaks reports B's identifiers in an answer that the request did not
	// itself carry.
	leaks := func(c isolationCaller, rt route, sent string, rec *httptest.ResponseRecorder) {
		t.Helper()
		answer := rec.Body.String() + fmt.Sprint(rec.Header())
		for _, marker := range b.markers() {
			if strings.Contains(answer, marker) && !strings.Contains(sent, marker) &&
				!strings.Contains(sent, url.QueryEscape(marker)) {
				t.Errorf("%s: %s %s answered %d with B's %q in it: %s",
					c.who, rt.Method, sent, rec.Code, marker, strings.TrimSpace(rec.Body.String()))
			}
		}
	}
	// compare is the oracle: what A meets naming B is what it meets naming
	// nothing. A refusal is compared whole; a success by its status and the
	// fragment that makes it the right one; a listing whole.
	compare := func(c isolationCaller, rt route, label string, recB, recGhost *httptest.ResponseRecorder, carries string) {
		t.Helper()
		if recB.Code != recGhost.Code {
			t.Errorf("%s: %s %s (%s) = %d for B, %d for nothing: the answer tells B's row from none (%s | %s)",
				c.who, rt.Method, rt.Path, label, recB.Code, recGhost.Code,
				strings.TrimSpace(recB.Body.String()), strings.TrimSpace(recGhost.Body.String()))
			return
		}
		// A refusal is compared whole, and so is a listing's answer: the
		// same rows of A's, whatever the filter named.
		if recB.Code >= 400 || rt.Method == "GET" && !strings.Contains(rt.Path, "{") {
			if nb, ng := normalB(recB.Body.String()), normalGhost(recGhost.Body.String()); nb != ng {
				t.Errorf("%s: %s %s (%s): B's answer and nothing's differ:\n  B:     %s\n  ghost: %s",
					c.who, rt.Method, rt.Path, label, strings.TrimSpace(nb), strings.TrimSpace(ng))
			}
		}
		if carries != "" {
			for name, rec := range map[string]*httptest.ResponseRecorder{"B": recB, "ghost": recGhost} {
				if !strings.Contains(rec.Body.String(), carries) {
					t.Errorf("%s: %s %s (%s) for %s = %d %s, want it to carry %s",
						c.who, rt.Method, rt.Path, label, name, rec.Code, strings.TrimSpace(rec.Body.String()), carries)
				}
			}
		}
	}
	mayCall := func(rt route, c isolationCaller) bool { return decision(rt, c.who) == admitted }

	// walked counts the paired calls each row made, against what the matrix
	// plans for it below.
	walked := map[string]int{}
	// pair calls a route twice, once naming B and once naming the ghost,
	// with the tenants `from` assigns: `foreign` stands for whichever of the
	// two the call is about.
	pair := func(c isolationCaller, rt route, carries, label string,
		from func(foreign *tenant) map[string]*tenant, query string, body func(foreign, own *tenant) any,
	) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
		t.Helper()
		var recs [2]*httptest.ResponseRecorder
		for i, foreign := range []*tenant{b, ghost} {
			path, ok := pathWith(rt.Path, from(foreign))
			if !ok {
				t.Fatalf("%s %s: the matrix cannot fill its path values", rt.Method, rt.Path)
			}
			q := foreign.expand(query)
			payload := encode(body, foreign)
			recs[i] = send(c, rt, path, q, payload)
			if i == 0 {
				leaks(c, rt, path+"?"+q+" "+string(payload), recs[i])
			}
		}
		compare(c, rt, label, recs[0], recs[1], carries)
		walked[rt.Method+" "+rt.Path]++
		return recs[0], recs[1]
	}
	assign := func(rt route, foreignParams map[string]bool) func(*tenant) map[string]*tenant {
		return func(foreign *tenant) map[string]*tenant {
			from := map[string]*tenant{}
			for _, p := range pathParams(rt.Path) {
				from[p] = a
				if foreignParams[p] {
					from[p] = foreign
				}
			}
			return from
		}
	}
	label := func(foreignParams map[string]bool) string {
		names := make([]string, 0, len(foreignParams))
		for p := range foreignParams {
			names = append(names, p)
		}
		sort.Strings(names)
		return "foreign " + strings.Join(names, ", ")
	}
	upload := uploadFor(t, h, a)

	// Phase one: everything that names one of B's rows and must find none,
	// and every listing filtered by B's identifiers. Nothing here may
	// change a row anywhere, so the order does not matter.
	for _, rt := range routes {
		row := isolationMatrix[rt.Method+" "+rt.Path]
		for _, c := range callers {
			if !mayCall(rt, c) {
				continue
			}
			switch row.kind {
			case oneRow, projectScoped:
				for _, foreignParams := range pathVariants(rt.Path) {
					want := row.status
					if want == 0 {
						want = http.StatusNotFound
					}
					if row.kind == projectScoped {
						if foreignParams["id"] {
							// Decision 6: a project the caller is not
							// in, the same 403 for one that is not
							// there at all.
							want = http.StatusForbidden
						} else if row.action {
							continue // phase two
						}
					}
					query := row.query
					if rt.Path == "/api/public/media/{mediaId}/upload" {
						query = "token=" + url.QueryEscape(upload)
					}
					recB, _ := pair(c, rt, "", label(foreignParams), assign(rt, foreignParams), query, row.body)
					if recB.Code != want {
						t.Errorf("%s: %s %s (%s) = %d (%s), want %d",
							c.who, rt.Method, rt.Path, label(foreignParams), recB.Code,
							strings.TrimSpace(recB.Body.String()), want)
					}
				}
			case listing:
				path, _ := pathWith(rt.Path, nil)
				rec := send(c, rt, path, "", nil)
				leaks(c, rt, path, rec)
				if rec.Code != http.StatusOK {
					t.Errorf("%s: %s %s = %d (%s), want 200", c.who, rt.Method, path, rec.Code, rec.Body)
				}
				for _, probe := range row.probes {
					recB, _ := pair(c, rt, "", probe, func(*tenant) map[string]*tenant { return nil }, probe, nil)
					if want := cmp.Or(row.status, http.StatusOK); recB.Code != want {
						t.Errorf("%s: %s %s?%s = %d (%s), want %d and nothing of B's",
							c.who, rt.Method, rt.Path, b.expand(probe), recB.Code, strings.TrimSpace(recB.Body.String()), want)
					}
				}
			}
			// No route takes the project from the query: the strict
			// parser refuses a parameter it does not know, and a
			// project parameter is one nobody knows — before it looks
			// anything up, so B's real rows and the ghost's are
			// refused alike. Under A's own project: the guard has a
			// refusal of its own for a project the caller is not in.
			// The Langfuse routes read the SDK's requests leniently
			// (spec 002 #17), and their project is the key's alone.
			if row.kind != aboutPeople && strings.HasPrefix(rt.Path, "/api/v1/") {
				foreignParams := map[string]bool{}
				for _, p := range pathParams(rt.Path) {
					foreignParams[p] = !projectRoute(rt.Path) || p != "id"
				}
				for _, name := range []string{"project", "project_id"} {
					recB, _ := pair(c, rt, "", "?"+name+"=", assign(rt, foreignParams), name+"={project}", row.body)
					if recB.Code != http.StatusBadRequest || !strings.Contains(recB.Body.String(), "unknown query parameter") {
						t.Errorf("%s: %s %s?%s=<B> = %d (%s), want 400 for an unknown parameter", c.who, rt.Method,
							rt.Path, name, recB.Code, strings.TrimSpace(recB.Body.String()))
					}
				}
			}
		}
	}

	// A session naming B's project in the header meets the refusal it meets
	// for a project that is not there, on every route that reads the
	// header.
	for _, rt := range routes {
		row := isolationMatrix[rt.Method+" "+rt.Path]
		if row.kind == aboutPeople || rt.Policy == ingest || rt.Policy == presigned || projectRoute(rt.Path) {
			continue
		}
		for _, c := range callers[1:] {
			path, _ := pathWith(rt.Path, assign(rt, nil)(a))
			var recs [2]*httptest.ResponseRecorder
			for i, named := range []string{b.projectID, ghost.projectID} {
				recs[i] = h.call(t, rt.Method, path, encode(row.body, a),
					asSession(map[who]*signedIn{editorSession: editor, viewerSession: viewer}[c.who]),
					inProject(named), asJSON)
			}
			if recs[0].Code != http.StatusForbidden || recs[0].Body.String() != recs[1].Body.String() {
				t.Errorf("%s naming B: %s %s = %d (%s), naming nothing = %d (%s); want the same 403",
					c.who, rt.Method, path, recs[0].Code, strings.TrimSpace(recs[0].Body.String()),
					recs[1].Code, strings.TrimSpace(recs[1].Body.String()))
			}
		}
	}

	// Phase two: the writes that may make A rows under B's names and ids.
	// Each is made naming B and then naming the ghost, by the same caller in
	// the same state, so the two answers still compare.
	for _, rt := range routes {
		row := isolationMatrix[rt.Method+" "+rt.Path]
		for _, c := range callers {
			if !mayCall(rt, c) {
				continue
			}
			own := func(*tenant) map[string]*tenant { return assign(rt, nil)(a) }
			if row.kind == createsByName {
				pair(c, rt, "", "created by B's name", func(f *tenant) map[string]*tenant {
					return map[string]*tenant{"name": f}
				}, row.query, row.body)
			}
			if row.kind == projectScoped && row.action {
				foreignParams := map[string]bool{}
				for _, p := range pathParams(rt.Path)[1:] {
					foreignParams[p] = true
				}
				pair(c, rt, "", "acting on A's project", assign(rt, foreignParams), row.query, row.body)
			}
			for i, ref := range row.refs {
				body := ref.body
				if body == nil {
					body = row.body
				}
				recB, _ := pair(c, rt, ref.carries, fmt.Sprintf("ref %d", i), own, ref.query, body)
				if recB.Code >= 400 && recB.Code != http.StatusNotFound && recB.Code != http.StatusUnprocessableEntity {
					t.Errorf("%s: %s %s (ref %d) = %d (%s): a call that names B's ids must be judged on them, not refused as malformed",
						c.who, rt.Method, rt.Path, i, recB.Code, strings.TrimSpace(recB.Body.String()))
				}
			}
		}
	}
	// Every row made the calls the matrix plans for it, and every row that
	// is about a project's rows made some — or says why none of A's three
	// may call it.
	total := 0
	for _, rt := range routes {
		key := rt.Method + " " + rt.Path
		row := isolationMatrix[key]
		want := 0
		for _, c := range callers {
			if !mayCall(rt, c) {
				continue
			}
			want += plannedPairs(rt, row)
		}
		if walked[key] != want {
			t.Errorf("%s made %d paired calls, the matrix plans %d", key, walked[key], want)
		}
		if want == 0 && row.kind != aboutPeople && row.why == "" {
			t.Errorf("%s: none of A's key, editor and viewer may call it, and the row does not say why", key)
		}
		total += walked[key]
	}
	t.Logf("%d paired calls", total)

	// B's rows are as they were, column by column, and by B's own eyes.
	after := fingerprint(t, h, b.projectID)
	for table, rows := range before {
		if !slices.Equal(rows, after[table]) {
			t.Errorf("B's %s changed:\n  before %v\n  after  %v", table, rows, after[table])
		}
	}
	for table, rows := range after {
		if _, ok := before[table]; !ok && len(rows) > 0 {
			t.Errorf("B gained rows in %s: %v", table, rows)
		}
	}
	// The routes about people refuse the parameter too, as every caller
	// they admit — anonymously for a public one, an owner for the accounts.
	// Last, and each with fresh sessions, because two of them end sessions.
	owner := h.owner(t)
	ghostAccount, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range routes {
		if isolationMatrix[rt.Method+" "+rt.Path].kind != aboutPeople || !strings.HasPrefix(rt.Path, "/api/v1") {
			continue
		}
		path := strings.NewReplacer("{id}", ghostAccount, "{project_id}", ghost.projectID).Replace(rt.Path)
		askers := map[who]func(*http.Request){}
		if rt.Policy == public {
			askers[noCredential] = anonymous
		} else {
			sessions := map[who]*signedIn{editorSession: editor, viewerSession: viewer, ownerSession: owner}
			for _, w := range []who{projectKey, editorSession, viewerSession, ownerSession} {
				if decision(rt, w) != admitted {
					continue
				}
				askers[w] = func(*http.Request) {}
				if person, ok := sessions[w]; ok {
					askers[w] = asSession(h.resume(t, person, "probe-"+rt.Method+rt.Path))
				}
			}
		}
		if len(askers) == 0 {
			t.Errorf("%s %s: nobody may call it to ask", rt.Method, rt.Path)
		}
		for w, ask := range askers {
			for _, name := range []string{"project", "project_id"} {
				rec := h.call(t, rt.Method, path+"?"+name+"="+b.projectID, []byte(`{}`), ask, asJSON)
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown query parameter") {
					t.Errorf("%s: %s %s?%s=<B> = %d (%s), want 400 for an unknown parameter", w, rt.Method, path,
						name, rec.Code, strings.TrimSpace(rec.Body.String()))
				}
			}
		}
	}

	if rec := h.call(t, "GET", "/api/v1/traces?q="+b.content, nil, asKey(bSecret)); !strings.Contains(rec.Body.String(), b.trace) {
		t.Errorf("B's own search for its content = %d %s, want its trace", rec.Code, rec.Body)
	}
	for _, path := range []string{
		"/api/v1/traces/" + b.trace, "/api/v1/scores/" + b.score, "/api/v1/prompts/" + b.prompt,
		"/api/v1/datasets/" + b.dataset + "/items/" + b.item, "/api/v1/runs/" + b.run,
		"/api/v1/score-configs/" + b.config, "/api/v1/queues/" + b.queue + "/items/" + b.queueItem,
		"/api/v1/media/" + b.sha, "/api/public/media/" + b.mediaID, "/api/v1/raw/" + b.raw,
		"/api/v1/projects/" + b.projectID + "/erasures/" + b.erasure,
	} {
		expectStatus(t, h.call(t, "GET", path, nil, asKey(bSecret)), http.StatusOK)
	}

	// A's archive is numbered by A's batches alone (spec 019 #17). The id
	// probes above name B's batch; this is what A learns without naming
	// anything: B's exports between two of A's leave no gap in A's numbers,
	// so the ids A reads say nothing of how much B sent, or when.
	export := func(tn *tenant, secret string) {
		t.Helper()
		expectStatus(t, h.call(t, "POST", "/v1/traces", ingestExport(tn, "numbering", nil), asKey(secret),
			func(r *http.Request) { r.Header.Set("Content-Type", "application/x-protobuf") }), http.StatusOK)
	}
	export(a, testSecret)
	for range 3 {
		export(b, bSecret)
	}
	export(a, testSecret)
	newest := decodeJSON[rawListing](t, h.get(t, "/api/v1/raw?direction=prev&limit=2")).Batches
	if len(newest) != 2 || newest[1].ID != newest[0].ID+1 {
		t.Errorf("A's two newest batches = %+v, want consecutive ids: B's three exports between them are no gap", newest)
	}

	// A neighbour's traffic is not news to A (spec 004 #37). Everything
	// above names B's rows; this is the other channel, the one no
	// identifier crosses: a number of A's that moves when B ingests or
	// deletes. Every listing A's three callers read answers the same before
	// B exports and deletes its traces as after — once the process's own
	// clock is taken out — and so `/system` cannot carry the file's growth,
	// the queue, the budget or a compaction B asked for. Last, because the
	// deletion takes B's traces.
	quiet := func() map[string]string {
		answers := map[string]string{}
		for _, rt := range routes {
			if isolationMatrix[rt.Method+" "+rt.Path].kind != listing {
				continue
			}
			for _, c := range callers {
				if !mayCall(rt, c) {
					continue
				}
				path, _ := pathWith(rt.Path, nil)
				rec := send(c, rt, path, "", nil)
				answers[c.who.String()+" "+rt.Method+" "+path] = fmt.Sprint(rec.Code, " ",
					withoutClock(t, rt.Method+" "+rt.Path, rec.Body.Bytes()))
			}
		}
		return answers
	}
	still := quiet()
	expectStatus(t, h.call(t, "POST", "/v1/traces", ingestExport(b, "traffic", nil), asKey(bSecret),
		func(r *http.Request) { r.Header.Set("Content-Type", "application/x-protobuf") }), http.StatusOK)
	expectStatus(t, h.call(t, "DELETE", "/api/v1/traces?to="+farFuture+"&confirm="+bProjectName, nil,
		asKey(bSecret)), http.StatusOK)
	for asked, answer := range quiet() {
		if answer != still[asked] {
			t.Errorf("%s moved when B exported and deleted:\n  before %s\n  after  %s", asked, still[asked], answer)
		}
	}
}

// clockFields are the fields of a quiet listing that move with the clock and
// nobody's traffic, by route, each with the reason. Everything else is
// compared exactly. The sweeper's `next_run` and `last_run` are not here: they
// move when a pass runs, and none runs between the two readings.
var clockFields = map[string][]struct {
	field, why string
}{
	"GET /api/v1/system": {{"uptime_seconds", "how long the process has been up"}},
	"GET /api/v1/facets": {{"to", "the window's end, which defaults to the moment of asking"}},
}

// withoutClock is a listing's answer with its route's clockFields taken out.
func withoutClock(t *testing.T, route string, body []byte) string {
	t.Helper()
	fields := clockFields[route]
	if len(fields) == 0 {
		return string(body)
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("%s: %v", route, err)
	}
	for _, f := range fields {
		if _, ok := answer[f.field]; !ok {
			t.Errorf("%s has no %s to take out (%s)", route, f.field, f.why)
		}
		delete(answer, f.field)
	}
	return string(mustJSONBytes(answer))
}

// uploadFor is a live upload token of A's, for a body A has not stored: the
// presigned PUT is tried with it against B's media id and the ghost's.
func uploadFor(t *testing.T, h *harness, own *tenant) string {
	t.Helper()
	picture := testPicture(333, 99)
	rec := h.call(t, "POST", "/api/public/media", mustJSON(t, mediaAsk(picture, own.trace)), asKey(own.secret), asJSON)
	expectStatus(t, rec, http.StatusOK)
	answer := decodeJSON[struct {
		UploadURL *string `json:"uploadUrl"`
	}](t, rec)
	if answer.UploadURL == nil {
		t.Fatal("no upload URL for a body A does not hold")
	}
	parsed, err := url.Parse(*answer.UploadURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query().Get("token")
}

// farFuture is a bulk deletion's `to`: every trace there is.
const farFuture = "2100-01-01T00:00:00Z"

const (
	bProjectName = "other"
	bSecret      = "tp-sk-other"
)

func mustJSONBytes(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// TestAReferenceStringIsNotProofOfPossession: B's media id in a span of A's —
// the reference string the Langfuse SDK leaves in a payload — makes A no
// holder of B's body. The bytes are the proof (spec 041 #9); a string anyone
// who saw one of B's payloads could copy is not, so A's read keeps the string
// as sent and the body stays B's alone.
func TestAReferenceStringIsNotProofOfPossession(t *testing.T) {
	h := newAccountHarness(t)
	b := newTenant("bravo", bSecret, h.second(t, bProjectName, bSecret).ID, "tp-pk-"+bProjectName, 2)
	h.seedTenant(t, b)

	reference := mapping.LangfuseMarker + "type=image/png|id=" + b.mediaID + "|source=bytes@@@"
	span := otlptest.ProbeSpan("langfuse.observation.input", reference)
	span.TraceId, _ = hex.DecodeString(traceHex(7))
	span.SpanId, _ = hex.DecodeString(spanHex(7))
	expectStatus(t, h.post(t, "/v1/traces", encodeExport(t, otlptest.Export(span))), http.StatusOK)
	// And the SDK's own ask for B's body, for that trace: answered with an
	// upload URL, which is a request for the bytes A does not have.
	rec := h.call(t, "POST", "/api/public/media", mustJSON(t, mediaAsk(b.picture, traceHex(7))), asJSON)
	expectStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"uploadUrl":"http`) {
		t.Fatalf("A's ask for B's body = %s, want an upload URL", rec.Body)
	}

	rec = h.get(t, "/api/v1/observations/"+spanHex(7)+"/io?trace_id="+traceHex(7))
	expectStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), b.sha) || !strings.Contains(rec.Body.String(), b.mediaID) {
		t.Errorf("A's input = %s, want B's reference string as sent and no body of B's", rec.Body)
	}
	expectStatus(t, h.get(t, "/api/v1/media/"+b.sha), http.StatusNotFound)
	expectStatus(t, h.get(t, "/api/public/media/"+b.mediaID), http.StatusNotFound)
	if h.mediaHeldBy(t, h.project.ID, b.sha) {
		t.Error("A holds B's body without having sent it")
	}
}

// ownersOnly is why a project's route is called by none of A's three: an owner
// reaches every project by design, and nobody else may call it.
const ownersOnly = "an owner's: an owner reaches every project, and none of A's key, editor and viewer may call it"
