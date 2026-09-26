package store

import (
	"errors"
	"sync"
	"time"
)

// The database's conditions (spec 043 #2): failures that pass on their own —
// a lock that did not clear, a full disk, an I/O error — as against a failure
// the request's own content caused, which would fail again on every retry.
// SQLite's primary result codes, so that a caller above this package can tell
// the two apart without importing the driver.
const (
	sqliteBusy     = 5
	sqliteLocked   = 6
	sqliteNoMem    = 7
	sqliteIOErr    = 10
	sqliteFull     = 13
	sqliteCantOpen = 14
)

var conditionNames = map[int]string{
	sqliteBusy:     "SQLITE_BUSY",
	sqliteLocked:   "SQLITE_LOCKED",
	sqliteNoMem:    "SQLITE_NOMEM",
	sqliteIOErr:    "SQLITE_IOERR",
	sqliteFull:     "SQLITE_FULL",
	sqliteCantOpen: "SQLITE_CANTOPEN",
}

// Condition names the database condition err is, with its extended code
// folded into the primary one, and reports whether it is one at all. The
// driver's error is recognised by the one method it carries, `Code() int`,
// anywhere in the chain.
func Condition(err error) (string, bool) {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return "", false
	}
	name, ok := conditionNames[coded.Code()&0xff]
	return name, ok
}

// conditionLog paces the log lines of commits a database condition failed:
// one a minute per condition, saying how many it stands for (spec 043 #2). A
// full disk fails every write until it is freed, and a line per export would
// bury the one that says so.
var conditionLog = &pacedLog{every: time.Minute}

type pacedLog struct {
	mu    sync.Mutex
	every time.Duration
	last  map[string]time.Time
	held  map[string]int64
}

// allow reports whether to log the key now, and how many lines were held back
// since the last one that was.
func (l *pacedLog) allow(key string, now time.Time) (skipped int64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last, l.held = map[string]time.Time{}, map[string]int64{}
	}
	if last, seen := l.last[key]; seen && now.Sub(last) < l.every {
		l.held[key]++
		return 0, false
	}
	skipped = l.held[key]
	l.last[key], l.held[key] = now, 0
	return skipped, true
}
