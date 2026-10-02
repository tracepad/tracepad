// Package rawid is the one definition of how the raw archive names a batch
// and a position in it (spec 019 #17), for the server that issues both and the
// CLI that rebuilds a resume cursor from a listing row.
//
// A batch's id is its number within its project with a tag in front: `n42`.
// The tag is what makes an id from before the numbering — the table's rowid,
// a bare integer shared by every tenant — invalid by construction rather than
// by luck: `42` and `n42` cannot be mistaken for each other, so a recorded old
// id is refused instead of naming whichever batch of the project now has that
// number.
//
// A cursor is base64url (no padding) of `n:<received_at>:<number>`, the time
// in Unix nanoseconds. One from before is `<received_at>:<rowid>`, which
// Parse reports as a *LegacyCursor carrying the instant it was at.
package rawid

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Tag opens every batch id and every cursor.
const Tag = "n"

// ID is a batch's id: its number within its project, tagged.
func ID(number int64) string { return Tag + strconv.FormatInt(number, 10) }

// ErrNotAnID is an id that is not in this grammar and not a pre-numbering one
// either.
var ErrNotAnID = errors.New("not a raw batch id")

// LegacyID is a bare integer: an id from before batches were numbered within
// their project.
type LegacyID struct{ Value string }

func (e *LegacyID) Error() string {
	return fmt.Sprintf("raw batch id %q is from before raw batches were numbered within their project, "+
		"and no longer names a batch; list the archive again for the batch's id", e.Value)
}

// ParseID reads a batch id, returning its number. A bare integer is a
// *LegacyID; anything else that is not `n<positive integer>` is ErrNotAnID.
func ParseID(value string) (int64, error) {
	if rest, tagged := strings.CutPrefix(value, Tag); tagged {
		number, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || number < 1 || rest != strconv.FormatInt(number, 10) {
			return 0, ErrNotAnID
		}
		return number, nil
	}
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return 0, &LegacyID{Value: value}
	}
	return 0, ErrNotAnID
}

// Cursor is the position of one batch: its arrival and its number.
func Cursor(receivedAt, number int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(
		Tag + ":" + strconv.FormatInt(receivedAt, 10) + ":" + strconv.FormatInt(number, 10)))
}

// ErrNotACursor is a cursor in no grammar this archive ever issued.
var ErrNotACursor = errors.New("invalid cursor")

// LegacyCursor is a cursor from before the numbering: it names its batch by a
// rowid that no longer leaves the server, and can only say the instant it was
// at.
type LegacyCursor struct{ ReceivedAt int64 }

func (e *LegacyCursor) Error() string {
	return "this cursor is from before raw batches were numbered within their project, and no longer names a batch"
}

// ParseCursor reads a cursor into its arrival and number.
func ParseCursor(value string) (receivedAt, number int64, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, 0, ErrNotACursor
	}
	parts := strings.Split(string(raw), ":")
	switch {
	case len(parts) == 3 && parts[0] == Tag:
		receivedAt, errAt := strconv.ParseInt(parts[1], 10, 64)
		number, errNumber := strconv.ParseInt(parts[2], 10, 64)
		if errAt != nil || errNumber != nil {
			return 0, 0, ErrNotACursor
		}
		return receivedAt, number, nil
	case len(parts) == 2:
		receivedAt, errAt := strconv.ParseInt(parts[0], 10, 64)
		_, errID := strconv.ParseInt(parts[1], 10, 64)
		if errAt != nil || errID != nil {
			return 0, 0, ErrNotACursor
		}
		return 0, 0, &LegacyCursor{ReceivedAt: receivedAt}
	}
	return 0, 0, ErrNotACursor
}
