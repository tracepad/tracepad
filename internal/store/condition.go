package store

import (
	"errors"
	"time"

	"github.com/tracepad/tracepad/internal/logpace"
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

// Primary codes that are not conditions — they do not pass on their own —
// but that an erasure names as a cause, since an operator acts on them
// (spec 047 #32).
const (
	sqliteReadOnly = 8
	sqliteCorrupt  = 11
	sqliteNotADB   = 26
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
	code, ok := sqliteCode(err)
	if !ok {
		return "", false
	}
	name, ok := conditionNames[code]
	return name, ok
}

// sqliteCode is the primary result code of the driver's error in err's
// chain, its extended code folded in, and whether there is one.
func sqliteCode(err error) (int, bool) {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return 0, false
	}
	return coded.Code() & 0xff, true
}

// conditionLog paces the log lines of commits a database condition failed:
// one a minute per condition, saying how many it stands for (spec 043 #2). A
// full disk fails every write until it is freed, and a line per export would
// bury the one that says so.
var conditionLog = &logpace.Keyed{Every: time.Minute}
