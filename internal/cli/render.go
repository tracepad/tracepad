package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
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

func (t *table) row(cells ...string) {
	fmt.Fprintln(t.writer, strings.Join(cells, "\t"))
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
	Tags             []string `json:"tags"`
	Timestamp        string   `json:"timestamp"`
	TotalCost        *float64 `json:"total_cost"`
	LatencyMs        *int64   `json:"latency_ms"`
	ErrorCount       int      `json:"error_count"`
	ObservationCount int      `json:"observation_count"`
}

// observationNode is one node of the tree the trace endpoint returns.
type observationNode struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	Name          string            `json:"name"`
	StartTime     string            `json:"start_time"`
	EndTime       string            `json:"end_time"`
	Model         string            `json:"model"`
	Level         string            `json:"level"`
	StatusMessage string            `json:"status_message"`
	Usage         map[string]any    `json:"usage"`
	CostDetails   map[string]any    `json:"cost_details"`
	Input         json.RawMessage   `json:"input"`
	Output        json.RawMessage   `json:"output"`
	Metadata      json.RawMessage   `json:"metadata"`
	Children      []observationNode `json:"children"`
}

// traceDetail is one trace with its tree.
type traceDetail struct {
	traceRow
	Metadata     map[string]any    `json:"metadata"`
	Observations []observationNode `json:"observations"`
}

func renderTraceTable(out io.Writer, rows []traceRow) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "no traces")
		return
	}
	t := newTable(out, "TIME", "ID", "NAME", "ENV", "OBS", "ERR", "LATENCY", "COST")
	for _, row := range rows {
		t.row(
			shortTime(row.Timestamp),
			row.ID,
			orDash(row.Name),
			orDash(row.Environment),
			strconv.Itoa(row.ObservationCount),
			strconv.Itoa(row.ErrorCount),
			duration(row.LatencyMs),
			cost(row.TotalCost),
		)
	}
	t.flush()
}

func renderTraceDetail(out io.Writer, trace traceDetail) {
	fmt.Fprintf(out, "trace %s\n", trace.ID)
	fmt.Fprintf(out, "  name        %s\n", orDash(trace.Name))
	fmt.Fprintf(out, "  environment %s\n", orDash(trace.Environment))
	if trace.UserID != "" {
		fmt.Fprintf(out, "  user        %s\n", trace.UserID)
	}
	if trace.SessionID != "" {
		fmt.Fprintf(out, "  session     %s\n", trace.SessionID)
	}
	if len(trace.Tags) > 0 {
		fmt.Fprintf(out, "  tags        %s\n", strings.Join(trace.Tags, ", "))
	}
	fmt.Fprintf(out, "  started     %s\n", shortTime(trace.Timestamp))
	fmt.Fprintf(out, "  latency     %s\n", duration(trace.LatencyMs))
	fmt.Fprintf(out, "  cost        %s\n", cost(trace.TotalCost))
	fmt.Fprintf(out, "  errors      %d of %d observations\n", trace.ErrorCount, trace.ObservationCount)
	if len(trace.Metadata) > 0 {
		fmt.Fprintf(out, "  metadata    %s\n", compact(trace.Metadata))
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
	if tokens := totalTokens(node.Usage); tokens != "" {
		parts = append(parts, tokens)
	}
	if amount := detailCost(node.CostDetails); amount != "" {
		parts = append(parts, amount)
	}
	fmt.Fprintf(out, "%s%s %s  [%s]\n", indent, marker, strings.Join(parts, "  "), node.ID)
	if node.StatusMessage != "" {
		fmt.Fprintf(out, "%s    status: %s\n", indent, node.StatusMessage)
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
		fmt.Fprintf(out, "%s    %s: %s\n", indent, payload.label, payloadText(payload.value))
	}
	for _, child := range node.Children {
		renderObservation(out, child, indent+"  ")
	}
}

// payloadText renders a payload, or what a truncation marker says about the
// one that did not fit. The marker is not noise to hide: it is the affordance
// that says the rest is one command away.
func payloadText(raw json.RawMessage) string {
	var marker struct {
		Truncated bool   `json:"truncated"`
		Size      int    `json:"size"`
		Preview   string `json:"preview"`
		Full      string `json:"full"`
	}
	if err := json.Unmarshal(raw, &marker); err == nil && marker.Truncated {
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
		return value
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
