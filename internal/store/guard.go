package store

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// PanicError is what a panic in a write job or in a background worker's pass
// becomes once it is recovered (spec 043 #42). It says where, and never what:
// a panic's value may quote what the code held.
type PanicError struct {
	Where string
}

func (e *PanicError) Error() string { return "panic in " + e.Where }

// recovered logs a recovered panic — where, the type of its value, the
// message of a runtime error (an index or a nil pointer names no data), and the
// stack — and returns the error that stands for it. Any other value's text is
// left out: an erasure's code can panic with an error that quotes its user
// (spec 047 #33), and nothing filters a log line.
func recovered(where string, value any) error {
	attrs := []any{"where", where, "type", fmt.Sprintf("%T", value)}
	if fault, ok := value.(runtime.Error); ok {
		attrs = append(attrs, "message", fault.Error())
	}
	attrs = append(attrs, "stack", string(debug.Stack()))
	logger().Error("a panic was recovered: the work in hand is abandoned and the process goes on", attrs...)
	return &PanicError{Where: where}
}

// guardLabel runs fn, and answers a panic in it as an error. The label is
// asked for only when there is a panic to name, so a call that does not panic
// pays for no formatting.
func guardLabel(label func() string, fn func() error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = recovered(label(), value)
		}
	}()
	return fn()
}

// guard runs fn under a fixed label. A background worker's pass runs under
// it, so that a bug met on odd data costs that pass — the next tick or the
// next start tries again — and not the process, with its ingest and its
// interface.
func guard(where string, fn func() error) error {
	return guardLabel(func() string { return where }, fn)
}

// guardJob is guard for the code of a job, labelled by its type.
func guardJob(what string, job any, fn func() error) error {
	return guardLabel(func() string { return fmt.Sprintf("%s %T", what, job) }, fn)
}

// try is guard for a step that answers a value as well: after a panic the
// value is the zero one.
func try[T any](where string, fn func() (T, error)) (T, error) {
	var out T
	err := guard(where, func() (err error) {
		out, err = fn()
		return err
	})
	return out, err
}

// panicLedger counts, per project, the passes in a row whose part for that
// project panicked. A project that panics maxHeldPasses times in a row is left
// out until the process restarts, as a failing hour is given up on after the
// same number (spec 043 #8, #42): a bug in the code that reads its data would
// otherwise log a stack every tick for ever. A pass in which it does not panic
// starts the count again.
type panicLedger struct {
	mu   sync.Mutex
	runs map[string]int
}

// skip says the project has been given up on.
func (l *panicLedger) skip(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.runs[id] >= maxHeldPasses
}

// settle records how a project's part of a pass ended, and reports whether it
// has just been given up on.
func (l *panicLedger) settle(id string, err error) (gaveUp bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !errors.As(err, new(*PanicError)) {
		delete(l.runs, id)
		return false
	}
	if l.runs == nil {
		l.runs = map[string]int{}
	}
	l.runs[id]++
	return l.runs[id] == maxHeldPasses
}
