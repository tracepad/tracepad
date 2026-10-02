// Package rawid is the one definition of how the raw archive names a batch
// and a position in it (spec 019 #17), for the server that issues both and the
// CLI that rebuilds a resume cursor from a listing row.
//
// A batch's id is its number within its project with a tag in front: `n42`.
// A cursor is base64url (no padding) of `n:<received_at>:<number>`, the time
// in Unix nanoseconds. Every number in either is written the one way
// strconv.FormatInt writes it — no sign, no leading zero — so that one
// position has one spelling, and anything else is refused: a bare integer
// cannot be taken for an id, nor a cursor in any other form for this one.
package rawid

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// Tag opens every batch id and every cursor.
const Tag = "n"

// ID is a batch's id: its number within its project, tagged.
func ID(number int64) string { return Tag + strconv.FormatInt(number, 10) }

// ErrNotAnID is a value that is not `n` followed by a positive number.
var ErrNotAnID = errors.New("not a raw batch id: an id is n and the batch's number, as the listing gives it; list the archive again")

// ParseID reads a batch id, returning its number.
func ParseID(value string) (int64, error) {
	rest, tagged := strings.CutPrefix(value, Tag)
	if !tagged {
		return 0, ErrNotAnID
	}
	number, ok := positive(rest)
	if !ok {
		return 0, ErrNotAnID
	}
	return number, nil
}

// Cursor is the position of one batch: its arrival and its number.
func Cursor(receivedAt, number int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(
		Tag + ":" + strconv.FormatInt(receivedAt, 10) + ":" + strconv.FormatInt(number, 10)))
}

// ErrNotACursor is a value no listing of this archive gave out.
var ErrNotACursor = errors.New("not a raw archive cursor: pass one a listing gave, or list the archive again")

// ParseCursor reads a cursor into its arrival and number.
func ParseCursor(value string) (receivedAt, number int64, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	// One spelling: the decoder is lenient twice over — the last character
	// of an unpadded string may carry bits no byte uses, so `bjoxOjE`,
	// `bjoxOjF`, `bjoxOjG` and `bjoxOjH` read as one position, and it skips
	// a carriage return or a line feed wherever it finds one — so what it
	// read is written back and must be what was given (found by FuzzRawIDs).
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != value {
		return 0, 0, ErrNotACursor
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 || parts[0] != Tag {
		return 0, 0, ErrNotACursor
	}
	at, okAt := canonical(parts[1])
	n, okNumber := positive(parts[2])
	if !okAt || at < 0 || !okNumber {
		return 0, 0, ErrNotACursor
	}
	return at, n, nil
}

// canonical reads an integer written as FormatInt writes it, and nothing else.
func canonical(text string) (int64, bool) {
	value, err := strconv.ParseInt(text, 10, 64)
	return value, err == nil && strconv.FormatInt(value, 10) == text
}

// positive is canonical, above zero: a batch's number.
func positive(text string) (int64, bool) {
	value, ok := canonical(text)
	return value, ok && value > 0
}
