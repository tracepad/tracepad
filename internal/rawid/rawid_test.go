package rawid

import (
	"encoding/base64"
	"errors"
	"testing"
)

func TestIDs(t *testing.T) {
	for _, number := range []int64{1, 42, 9_999_992} {
		got, err := ParseID(ID(number))
		if err != nil || got != number {
			t.Errorf("ParseID(ID(%d)) = %d, %v", number, got, err)
		}
	}
	// A bare integer is not an id, whatever it once was.
	for _, bad := range []string{"42", "", "n", "n0", "n-1", "n01", "n+1", "x42", "n4.2", "N42", "42n", " n4"} {
		if _, err := ParseID(bad); !errors.Is(err, ErrNotAnID) {
			t.Errorf("ParseID(%q) = %v, want not an id", bad, err)
		}
	}
}

func TestCursors(t *testing.T) {
	at, number, err := ParseCursor(Cursor(1_788_220_800_001_000_000, 7))
	if err != nil || at != 1_788_220_800_001_000_000 || number != 7 {
		t.Errorf("round trip = %d %d %v", at, number, err)
	}
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	// One spelling for one position: a sign, a leading zero, a zero number
	// or another shape is not a cursor this archive gave out.
	for _, bad := range []string{"nonsense!", encode("1788220800001000000:4812"), encode("x:1:2"), encode("n:1"),
		encode("n:-5:1"), encode("n:+5:1"), encode("n:5:007"), encode("n:5:0"), encode("n:05:1"), encode("n:5:1:2"),
		encode("a:b")} {
		if _, _, err := ParseCursor(bad); !errors.Is(err, ErrNotACursor) {
			t.Errorf("ParseCursor(%q) = %v, want not a cursor", bad, err)
		}
	}
}
