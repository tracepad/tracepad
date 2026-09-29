package store

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// PanicError is what a panic in a write job or in a background worker's pass
// becomes once it is recovered (spec 043 #42). It says where, and never what:
// a panic's value may quote what the code held.
type PanicError struct {
	Where string
}

func (e *PanicError) Error() string { return "panic in " + e.Where }

// PanicStats is what /system says of the panics recovered since the process
// started (spec 043 #42). It names no project: a project's name is another
// tenant's business, and the log line has it.
type PanicStats struct {
	Recovered int64  // panics recovered
	LastWhere string // what the last one was in, without a project's name
	LastAt    int64  // Unix nanoseconds, zero for none
	GivenUp   int64  // projects and steps left out until the process restarts
}

var panics struct {
	mu       sync.Mutex
	stats    PanicStats
	lastLog  map[string]time.Time
	silenced map[string]int
}

// panicLogEvery is how often a stack for one place is written: a panic that
// repeats every tick is one line with its stack a quarter of an hour, and the
// rest counted in the line after.
var panicLogEvery = 15 * time.Minute

// Panics reports the panics recovered since the process started.
func Panics() PanicStats {
	panics.mu.Lock()
	defer panics.mu.Unlock()
	return panics.stats
}

// recovered records a recovered panic, logs it — where, the type of its value,
// the message of a runtime error (an index or a nil pointer names no data), and
// the stack, once in panicLogEvery for a place — and returns the error that
// stands for it. Any other value's text is left out: an erasure's code can
// panic with an error that quotes its user (spec 047 #33), and nothing filters
// a log line.
func recovered(where string, value any) error {
	now := time.Now()
	place := where
	if i := strings.IndexByte(where, ' '); i > 0 && strings.HasPrefix(where, "erasure ") {
		place = "erasure" // an id is not a place
	}
	panics.mu.Lock()
	panics.stats.Recovered++
	panics.stats.LastWhere, panics.stats.LastAt = place, now.UnixNano()
	if panics.lastLog == nil {
		panics.lastLog, panics.silenced = map[string]time.Time{}, map[string]int{}
	}
	quiet := now.Sub(panics.lastLog[place]) < panicLogEvery
	silenced := panics.silenced[place]
	if quiet {
		panics.silenced[place]++
	} else {
		panics.lastLog[place], panics.silenced[place] = now, 0
	}
	panics.mu.Unlock()
	if !quiet {
		attrs := []any{"where", where, "type", fmt.Sprintf("%T", value)}
		if fault, ok := value.(runtime.Error); ok {
			attrs = append(attrs, "message", fault.Error())
		}
		if silenced > 0 {
			attrs = append(attrs, "repeated_since_last_line", silenced)
		}
		attrs = append(attrs, "stack", string(debug.Stack()))
		logger().Error("a panic was recovered: the work in hand is abandoned and the process goes on", attrs...)
	}
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

// panicLedger counts, per key, the passes in a row whose part for that key
// panicked. A project or a shared step of the pass that panics maxHeldPasses
// times in a row is left out until the process restarts, as a failing hour is
// given up on after the same number (spec 043 #8, #42): a bug in the code that
// reads its data would otherwise cost a pass its later work every tick for
// ever. Only a pass in which it does not panic starts the count again; an
// error that is not a panic — a busy writer — changes nothing.
type panicLedger struct {
	mu   sync.Mutex
	runs map[string]int
}

const stepKey = "step:"

// skip says the key has been given up on.
func (l *panicLedger) skip(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.runs[key] >= maxHeldPasses
}

// settle records how a key's part of a pass ended, and reports whether it has
// just been given up on.
func (l *panicLedger) settle(key string, err error) (gaveUp bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case err == nil:
		delete(l.runs, key)
	case errors.As(err, new(*PanicError)):
		if l.runs == nil {
			l.runs = map[string]int{}
		}
		l.runs[key]++
		if l.runs[key] == maxHeldPasses {
			panics.mu.Lock()
			panics.stats.GivenUp++
			panics.mu.Unlock()
			return true
		}
	}
	return false
}

// forget drops the projects that are gone; the steps are never gone.
func (l *panicLedger) forget(live map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key := range l.runs {
		if !strings.HasPrefix(key, stepKey) && !live[key] {
			delete(l.runs, key)
		}
	}
}

// run is guard for one part of a pass that the ledger keeps count of: not run
// at all once given up on, and said so, once, when it is given up on. what
// names it in the line that says so, with the project's name when it is one's.
func (l *panicLedger) run(key, where, what string, fn func() error) error {
	if l.skip(key) {
		return nil
	}
	err := guard(where, fn)
	if l.settle(key, err) {
		logger().Error("gave up on a part of a background pass that panicked in every pass; it is left out until the server restarts",
			"part", what, "passes", maxHeldPasses)
	}
	return err
}

// tryLedger is run for a part that answers a value as well.
func tryLedger[T any](l *panicLedger, key, where, what string, fn func() (T, error)) (T, error) {
	var out T
	err := l.run(key, where, what, func() (err error) {
		out, err = fn()
		return err
	})
	return out, err
}

// isNilJob says a job is nothing: nil, or a nil pointer, map or slice held in
// the interface, whose methods the writer's loop would call on nothing.
func isNilJob(job WriteJob) bool {
	if job == nil {
		return true
	}
	switch v := reflect.ValueOf(job); v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	}
	return false
}
