package store

import "errors"

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
