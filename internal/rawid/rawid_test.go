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
	// A bare integer is an id from before the numbering, refused as such.
	var legacy *LegacyID
	if _, err := ParseID("42"); !errors.As(err, &legacy) || legacy.Value != "42" {
		t.Errorf("ParseID(42) = %v, want a legacy id", err)
	}
	for _, bad := range []string{"", "n", "n0", "n-1", "n01", "n+1", "x42", "n4.2", "N42", "42n"} {
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
	old := base64.RawURLEncoding.EncodeToString([]byte("1788220800001000000:4812"))
	var legacy *LegacyCursor
	if _, _, err := ParseCursor(old); !errors.As(err, &legacy) || legacy.ReceivedAt != 1_788_220_800_001_000_000 {
		t.Errorf("an old cursor = %v, want a legacy cursor at its instant", err)
	}
	for _, bad := range []string{"nonsense!", base64.RawURLEncoding.EncodeToString([]byte("x:1:2")),
		base64.RawURLEncoding.EncodeToString([]byte("n:1")), base64.RawURLEncoding.EncodeToString([]byte("a:b"))} {
		if _, _, err := ParseCursor(bad); !errors.Is(err, ErrNotACursor) {
			t.Errorf("ParseCursor(%q) = %v, want not a cursor", bad, err)
		}
	}
}
