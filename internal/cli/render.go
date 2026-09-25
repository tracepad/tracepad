package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tracepad/tracepad/internal/termsafe"
)

// Human-readable output (spec 004 #12). The JSON mode prints the API's bytes
// verbatim — that is what makes "one command, one tool call, one HTTP request"
// the same answer — so everything here is only about the other mode, the one a
// person reads on a terminal.

// table writes aligned columns.
type table struct {
	writer *tabwriter.Writer
}

func newTable(out io.Writer, headers ...string) *table {
	t := &table{writer: tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)}
	if len(headers) > 0 {
		fmt.Fprintln(t.writer, strings.Join(headers, "\t"))
	}
	return t
}

// row writes one row of cells, each made terminal-safe here rather than at
// every call site: a cell is where most of what the server sends is printed,
// and one tab in one of them would shift every column after it (#35).
func (t *table) row(cells ...string) {
	fmt.Fprintln(t.writer, strings.Join(termsafe.All(cells), "\t"))
}

// line writes a line that is not a row: one trailing cell, which tabwriter
// keeps out of the column widths above and below it.
func (t *table) line(text string) {
	fmt.Fprintln(t.writer, text)
}

func (t *table) flush() { t.writer.Flush() }

// traceRow is a listing row as the API renders it. Absent fields stay absent,
// so every pointer here means "the trace never carried this".
type traceRow struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	UserID           string   `json:"user_id"`
	SessionID        string   `json:"session_id"`
	Environment      string   `json:"environment"`
	Release          string   `json:"release"`
	Version          string   `json:"version"`
	Tags             []string `json:"tags"`
	Timestamp        string   `json:"timestamp"`
	TotalCost        *float64 `json:"total_cost"`
	LatencyMs        *int64   `json:"latency_ms"`
	TTFTMs           *int64   `json:"ttft_ms"`
	ErrorCount       int      `json:"error_count"`
	ObservationCount int      `json:"observation_count"`
	// Match is where a search hit, present only with `--search` (spec 011).
	Match *traceMatch `json:"match"`
}

// traceMatch is the `match` field of a row taken with a search.
type traceMatch struct {
	// ObservationID is null when the trace's own name matched.
	ObservationID *string `json:"observation_id"`
	Field         string  `json:"field"`
	Snippet       string  `json:"snippet"`
}

// observationNode is one node of the tree the trace endpoint returns.
type observationNode struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	StartTime     string          `json:"start_time"`
	EndTime       string          `json:"end_time"`
	TTFTMs        *int64          `json:"ttft_ms"`
	Model         string          `json:"model"`
	Level         string          `json:"level"`
	StatusMessage string          `json:"status_message"`
	Usage         map[string]any  `json:"usage"`
	CostDetails   map[string]any  `json:"cost_details"`
	Prompt        *promptLink     `json:"prompt"`
	InputBytes    *int64          `json:"input_bytes"`
	OutputBytes   *int64          `json:"output_bytes"`
	Input         json.RawMessage `json:"input"`
	Output        json.RawMessage `json:"output"`
	Metadata      json.RawMessage `json:"metadata"`

	Children []observationNode `json:"children"`
}

// promptLink is the prompt an observation ran, as the client labelled it.
type promptLink struct {
	Name    string `json:"name"`
	Version *int64 `json:"version"`
}

// String renders the label the way a person writes it into `--prompt`, so the
// line a reader sees is the query they would type next.
func (p promptLink) String() string {
	if p.Version == nil {
		return p.Name
	}
	return fmt.Sprintf("%s@%d", p.Name, *p.Version)
}

// traceDetail is one trace with its tree.
type traceDetail struct {
	traceRow
	Metadata     map[string]any    `json:"metadata"`
	Observations []observationNode `json:"observations"`
	// Expansion is present only when `--full` was refused because the
	// budget could not carry a marker for every payload.
	Expansion *struct {
		Payloads     int    `json:"payloads"`
		BudgetNeeded int    `json:"budget_needed"`
		Reason       string `json:"reason"`
	} `json:"expansion"`
}

// renderTraceTable prints a listing. With a search, each row gains a second
// line naming the field that matched and the snippet around the hit (spec 011,
// CLI contract) — dimmed on a terminal, so the table still reads as a table.
func renderTraceTable(out io.Writer, rows []traceRow, colour bool) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "no traces")
		return
	}
	t := newTable(out, "TIME", "ID", "NAME", "ENV", "OBS", "ERR", "LATENCY", "TTFT", "COST")
	for _, row := range rows {
		t.row(
			shortTime(row.Timestamp),
			row.ID,
			orDash(row.Name),
			orDash(row.Environment),
			strconv.Itoa(row.ObservationCount),
			strconv.Itoa(row.ErrorCount),
			duration(row.LatencyMs),
			// After latency, and formatted like it: the two are the
			// same kind of number and are read side by side (spec 012,
			// CLI contract).
			duration(row.TTFTMs),
			cost(row.TotalCost),
		)
		if row.Match != nil {
			// A line with no tab in it is one trailing cell, which
			// tabwriter leaves out of every column: the snippet cannot
			// widen the table it sits under.
			t.line(dim(termsafe.String(matchLine(*row.Match)), colour))
		}
	}
	t.flush()
}

// matchLine is the second line under a matching row: where it hit, and what the
// text says around the hit.
func matchLine(match traceMatch) string {
	where := match.Field
	if match.ObservationID != nil {
		where = fmt.Sprintf("%s %s", *match.ObservationID, match.Field)
	}
	return fmt.Sprintf("    %s: %s", where, match.Snippet)
}

// dim renders text in the terminal's faint style, and plainly everywhere else.
// The text has been through termsafe already: these two sequences are the only
// escapes a human-mode run writes.
// The one place this binary colours anything: the snippet is annotation under a
// row, and on a terminal the difference between annotation and data is what
// keeps the table readable. Piped output is bytes somebody parses (#12), so it
// gets none of it.
func dim(text string, colour bool) string {
	if !colour {
		return text
	}
	return "\x1b[2m" + text + "\x1b[0m"
}

func renderTraceDetail(out io.Writer, trace traceDetail) {
	fmt.Fprintf(out, "trace %s\n", termsafe.String(trace.ID))
	fmt.Fprintf(out, "  name        %s\n", termsafe.String(orDash(trace.Name)))
	fmt.Fprintf(out, "  environment %s\n", termsafe.String(orDash(trace.Environment)))
	// Beside the environment, and only when the trace carried them: a
	// deployment nobody named is not a deployment called "-".
	if trace.Release != "" {
		fmt.Fprintf(out, "  release     %s\n", termsafe.String(trace.Release))
	}
	if trace.Version != "" {
		fmt.Fprintf(out, "  version     %s\n", termsafe.String(trace.Version))
	}
	if trace.UserID != "" {
		fmt.Fprintf(out, "  user        %s\n", termsafe.String(trace.UserID))
	}
	if trace.SessionID != "" {
		fmt.Fprintf(out, "  session     %s\n", termsafe.String(trace.SessionID))
	}
	if len(trace.Tags) > 0 {
		fmt.Fprintf(out, "  tags        %s\n", strings.Join(termsafe.All(trace.Tags), ", "))
	}
	fmt.Fprintf(out, "  started     %s\n", shortTime(trace.Timestamp))
	fmt.Fprintf(out, "  latency     %s\n", duration(trace.LatencyMs))
	fmt.Fprintf(out, "  ttft        %s\n", duration(trace.TTFTMs))
	fmt.Fprintf(out, "  cost        %s\n", cost(trace.TotalCost))
	fmt.Fprintf(out, "  errors      %d of %d observations\n", trace.ErrorCount, trace.ObservationCount)
	if len(trace.Metadata) > 0 {
		fmt.Fprintf(out, "  metadata    %s\n", termsafe.String(compact(trace.Metadata)))
	}
	if trace.Expansion != nil {
		// Saying nothing here would leave a reader of `--full` wondering
		// where the payloads went.
		fmt.Fprintf(out, "\n  %d payloads were not expanded: %s\n",
			trace.Expansion.Payloads, termsafe.String(trace.Expansion.Reason))
	}
	fmt.Fprintln(out)
	for _, node := range trace.Observations {
		renderObservation(out, node, "")
	}
}

// renderObservation prints one node and its subtree, indented by depth.
func renderObservation(out io.Writer, node observationNode, indent string) {
	marker := "·"
	if node.Level == "ERROR" {
		marker = "✗"
	}
	parts := []string{node.Type}
	if node.Name != "" {
		parts = append(parts, node.Name)
	}
	if node.Model != "" {
		parts = append(parts, node.Model)
	}
	if span := spanDuration(node.StartTime, node.EndTime); span != "" {
		parts = append(parts, span)
	}
	// The wait before the first token, named because a bare second duration
	// beside the span's would be unreadable (spec 012, CLI contract).
	if node.TTFTMs != nil {
		parts = append(parts, "ttft "+duration(node.TTFTMs))
	}
	if tokens := totalTokens(node.Usage); tokens != "" {
		parts = append(parts, tokens)
	}
	if amount := detailCost(node.CostDetails); amount != "" {
		parts = append(parts, amount)
	}
	if node.Prompt != nil {
		parts = append(parts, "prompt "+node.Prompt.String())
	}
	fmt.Fprintf(out, "%s%s %s  [%s]\n", indent, marker, termsafe.String(strings.Join(parts, "  ")), termsafe.String(node.ID))
	if node.StatusMessage != "" {
		fmt.Fprintf(out, "%s    status: %s\n", indent, termsafe.String(node.StatusMessage))
	}
	for _, payload := range []struct {
		label string
		value json.RawMessage
	}{
		{"input", node.Input},
		{"output", node.Output},
		{"metadata", node.Metadata},
	} {
		if len(payload.value) == 0 {
			continue
		}
		// JSON text escapes C0 and DEL but not C1, and a preview is
		// JSON text cut short.
		fmt.Fprintf(out, "%s    %s: %s\n", indent, payload.label, termsafe.String(payloadText(payload.value)))
	}
	for _, child := range node.Children {
		renderObservation(out, child, indent+"  ")
	}
}

// truncationMarker is what the server puts in place of a payload too big for
// the response budget (spec 004 #2), as much of it as a renderer needs.
type truncationMarker struct {
	Truncated bool   `json:"truncated"`
	Size      int    `json:"size"`
	Preview   string `json:"preview"`
	Full      string `json:"full"`
}

// truncationOf reads a marker out of a payload slot, and reports false for a
// payload that arrived whole — including one that is not an object at all, on
// which the decode simply fails.
func truncationOf(raw json.RawMessage) (truncationMarker, bool) {
	var marker truncationMarker
	if err := json.Unmarshal(raw, &marker); err != nil || !marker.Truncated {
		return truncationMarker{}, false
	}
	return marker, true
}

// payloadText renders a payload, or what a truncation marker says about the
// one that did not fit. The marker is not noise to hide: it is the affordance
// that says the rest is one command away.
func payloadText(raw json.RawMessage) string {
	if marker, cut := truncationOf(raw); cut {
		return fmt.Sprintf("%s… (%s truncated; whole payload at %s)",
			marker.Preview, byteSize(marker.Size), marker.Full)
	}
	return string(raw)
}

func byteSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// shortTime renders an RFC 3339 instant the way a log line reads. A value the
// server did not send stays a dash rather than becoming 1970.
func shortTime(value string) string {
	if value == "" {
		return "-"
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return termsafe.String(value)
	}
	return parsed.UTC().Format("2006-01-02 15:04:05")
}

func duration(ms *int64) string {
	if ms == nil {
		return "-"
	}
	if *ms < 1000 {
		return fmt.Sprintf("%dms", *ms)
	}
	return fmt.Sprintf("%.1fs", float64(*ms)/1000)
}

func spanDuration(start, end string) string {
	if start == "" || end == "" {
		return ""
	}
	from, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return ""
	}
	to, err := time.Parse(time.RFC3339Nano, end)
	if err != nil || to.Before(from) {
		return ""
	}
	ms := to.Sub(from).Milliseconds()
	return duration(&ms)
}

// cost prints what the client provided, and a dash when it provided nothing.
// A cost nobody reported is not zero (spec 002 #14).
func cost(value *float64) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("$%.6f", *value)
}

func detailCost(details map[string]any) string {
	total, ok := details["total"].(float64)
	if !ok {
		return ""
	}
	return fmt.Sprintf("$%.6f", total)
}

func totalTokens(usage map[string]any) string {
	total, ok := usage["total"].(float64)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d tokens", int64(total))
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func compact(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}
