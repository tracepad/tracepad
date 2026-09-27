package server

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sort"

	"github.com/tracepad/tracepad/internal/store"
)

/*
A trace's tree, bounded (spec 043 #18–#20).

The tree used to be exempt from every bound because a partial tree seemed a
wrong answer rather than a smaller one; but the number of a trace's
observations grows across exports without limit, each carries maps no budget
counts, and a chain of them nests two JSON levels per step. So:

  - the tree holds the longest prefix of the trace's observations, in
    (start_time, id) order, of at most 10,000 observations and 32 MiB of their
    own fields, and says how many it left out;
  - it is at most 100 levels deep, an observation at depth 101 detached to the
    root with its children, as a cycle's entry is;
  - it is written in one pass over one buffer, each observation's own fields
    rendered once as it is read, and under `?expand=io` each payload is read,
    cut to its share and written before the next is read.
*/

const (
	// maxTreeBytes bounds the observations' own fields in one tree: the
	// bound for a trace whose observations are few but crafted to be heavy.
	maxTreeBytes = 32 << 20
	// maxTreeDepth is how deep a tree nests: far deeper than any real call
	// stack of an application's steps, and far inside every JSON parser's
	// nesting limit at two levels an observation.
	maxTreeDepth = 100
)

// treeNode is one observation of a tree: its row, stripped of the maps it
// has already rendered, its own fields rendered, and the observations that
// named it as their parent.
type treeNode struct {
	row *store.ObservationRow
	// own is the observation's own fields as a rendered object — every field
	// but its payloads and its children, which are written after them.
	own      []byte
	children []*treeNode
}

// readTree reads the prefix of a trace's observations the tree holds, and
// renders each one's own fields as it arrives, so the maps a heavy observation
// carries are held once, as bytes.
func (s *Server) readTree(ctx context.Context, projectID, traceID string) ([]*treeNode, error) {
	var (
		nodes     []*treeNode
		size      int
		renderErr error
	)
	err := s.store.TreeObservations(ctx, projectID, traceID, store.MaxTreeObservations,
		func(row *store.ObservationRow) bool {
			var buffer bytes.Buffer
			if renderErr = renderOwn(row).appendJSON(&buffer); renderErr != nil {
				return false
			}
			if size+buffer.Len() > maxTreeBytes {
				return false
			}
			size += buffer.Len()
			// Rendered: what is left of the row is what the tree is
			// built from and the payloads are read by.
			row.ModelParameters, row.Usage, row.CostDetails = nil, nil, nil
			nodes = append(nodes, &treeNode{row: row, own: buffer.Bytes()})
			return true
		})
	if err == nil {
		err = renderErr
	}
	return nodes, err
}

// buildTree nests observations under their parents, siblings by start time.
// Rows arrive already ordered by (start_time, id), so appending preserves that
// order at every level.
//
// A span whose parent is not in this tree renders at the root with its
// `parent_observation_id` intact: the parent may still be in flight, and
// hiding the child until it lands would make a live trace look empty (edge
// cases), or it may be past the tree's bounds (spec 043 #18).
//
// A parent cycle is broken rather than merely re-rooted. `parent_span_id` is
// client bytes stored verbatim (spec 002), so two spans naming each other is
// something a caller can produce, and a cyclic `children` graph is not a
// rendering glitch — the renderer recurses into it until the goroutine stack
// is exhausted, which in Go is a fatal error the server cannot recover from
// (found in review of PR #5). The cycle's entry node is detached from its
// parent and rendered at the root, keeping every span visible and the graph
// finite. So is an observation deeper than maxTreeDepth (spec 043 #19).
func buildTree(nodes []*treeNode) []*treeNode {
	index := make(map[string]*treeNode, len(nodes))
	for _, node := range nodes {
		index[node.row.ID] = node
	}

	var roots []*treeNode
	for _, node := range nodes {
		parent, nested := index[node.row.ParentObservationID]
		if !nested || parent == node {
			roots = append(roots, node)
			continue
		}
		parent.children = append(parent.children, node)
	}

	// Anything unreachable from a root is in a cycle. Cutting the edge that
	// leads *into* such a node — rather than only adding it to the roots —
	// is what makes the result a tree: the node keeps its own children, so
	// nothing is lost, and its parent no longer points back at it.
	reachable := make(map[*treeNode]bool, len(nodes))
	walk := func(from *treeNode) {
		stack := []*treeNode{from}
		for len(stack) > 0 {
			node := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if reachable[node] {
				continue
			}
			reachable[node] = true
			stack = append(stack, node.children...)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	for _, node := range nodes {
		if reachable[node] {
			continue
		}
		if parent, nested := index[node.row.ParentObservationID]; nested {
			parent.children = slices.DeleteFunc(parent.children,
				func(child *treeNode) bool { return child == node })
		}
		roots = append(roots, node)
		walk(node)
	}

	roots = append(roots, detachDeep(roots)...)
	sort.SliceStable(roots, func(i, j int) bool {
		if roots[i].row.StartTime != roots[j].row.StartTime {
			return roots[i].row.StartTime < roots[j].row.StartTime
		}
		return roots[i].row.ID < roots[j].row.ID
	})
	return roots
}

// detachDeep cuts every observation that would sit deeper than maxTreeDepth
// from its parent and returns them, to be rendered at the root with their
// children — so a chain of a thousand steps reads as ten of a hundred, and
// every observation still appears once (spec 043 #19).
func detachDeep(roots []*treeNode) []*treeNode {
	type level struct {
		node  *treeNode
		depth int
	}
	var detached []*treeNode
	stack := make([]level, 0, len(roots))
	for _, root := range roots {
		stack = append(stack, level{root, 1})
	}
	for len(stack) > 0 {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if at.depth == maxTreeDepth {
			// Its children would sit one level too deep: each starts a
			// tree of its own.
			for _, child := range at.node.children {
				detached = append(detached, child)
				stack = append(stack, level{child, 1})
			}
			at.node.children = nil
			continue
		}
		for _, child := range at.node.children {
			stack = append(stack, level{child, at.depth + 1})
		}
	}
	return detached
}

// countPayloads counts the payload slots an expanded tree wants to inline —
// the divisor of the equal share every observation gets (spec 004 #6).
func countPayloads(nodes []*treeNode) int {
	slots := 0
	for _, node := range nodes {
		for _, kind := range store.PayloadKinds {
			if node.row.HasPayload(kind) {
				slots++
			}
		}
	}
	return slots
}

// treeJSON is the `observations` list of a trace, written in place: each
// observation's own fields as they were rendered when it was read, then — under
// `?expand=io` — its payloads, then its children.
type treeJSON struct {
	roots []*treeNode
	// payloads is nil unless the tree inlines them.
	payloads *treePayloads
}

func (t treeJSON) appendJSON(buffer *bytes.Buffer) error {
	return t.appendList(buffer, t.roots)
}

func (t treeJSON) appendList(buffer *bytes.Buffer, nodes []*treeNode) error {
	buffer.WriteByte('[')
	for i, node := range nodes {
		if i > 0 {
			buffer.WriteByte(',')
		}
		if err := t.appendNode(buffer, node); err != nil {
			return err
		}
	}
	buffer.WriteByte(']')
	return nil
}

func (t treeJSON) appendNode(buffer *bytes.Buffer, node *treeNode) error {
	// The own fields without their closing brace: the payloads and the
	// children are fields of the same object.
	buffer.Write(node.own[:len(node.own)-1])
	if t.payloads != nil {
		for _, kind := range store.PayloadKinds {
			if !node.row.HasPayload(kind) {
				continue
			}
			if err := t.payloads.append(buffer, node.row, kind); err != nil {
				return err
			}
		}
	}
	if len(node.children) > 0 {
		buffer.WriteString(`,"children":`)
		if err := t.appendList(buffer, node.children); err != nil {
			return err
		}
	}
	buffer.WriteByte('}')
	return nil
}

// treePayloads inlines an expanded tree's payloads one at a time: each is
// read, cut to its share and written before the next is read, so the answer's
// peak is the skeleton and the largest payload rather than the sum of them
// (spec 043 #18).
type treePayloads struct {
	ctx    context.Context
	reader *store.PayloadReader
	budget payloadBudget
}

// payloadReadFailed is a payload the store could not read, told apart from a
// rendering failure so that the handler answers it as a failed read.
type payloadReadFailed struct{ err error }

func (e *payloadReadFailed) Error() string { return e.err.Error() }
func (e *payloadReadFailed) Unwrap() error { return e.err }

func (p *treePayloads) append(buffer *bytes.Buffer, row *store.ObservationRow, kind store.PayloadKind) error {
	value, err := readPayload(p.reader, p.ctx, row, kind)
	if err != nil {
		return &payloadReadFailed{err}
	}
	defer payloadDone()
	if value == nil {
		return nil
	}
	buffer.WriteByte(',')
	key, err := json.Marshal(kind.Key())
	if err != nil {
		return err
	}
	buffer.Write(key)
	buffer.WriteByte(':')
	return appendValue(buffer, p.budget.render(value, row.TraceID, row.ID))
}

// readPayload is the payload reader's, a seam for the test that counts how
// many payloads a tree holds at once.
var readPayload = (*store.PayloadReader).Read

// payloadDone is told when a payload readPayload returned has been written and
// let go — a seam for the same test.
var payloadDone = func() {}
