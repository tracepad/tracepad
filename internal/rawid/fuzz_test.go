package rawid

import (
	"encoding/base64"
	"testing"
)

// FuzzRawIDs: an id and a cursor have one spelling each (the package's
// promise), so what is accepted is exactly what the formatter writes, and what
// the formatter writes is accepted. See internal/mapping/fuzz_test.go for how
// to run the fuzz targets.
func FuzzRawIDs(f *testing.F) {
	for _, seed := range []string{
		"", "n1", "n42", "n0", "n-1", "n01", "n+1", "42", "N42", "n9223372036854775807", "n9223372036854775808",
		Cursor(1, 1), Cursor(0, 1), Cursor(1787738400_000_000_000, 7), Cursor(-1, 1), Cursor(1, 0),
		base64.RawURLEncoding.EncodeToString([]byte("n:1:1:1")),
		base64.RawURLEncoding.EncodeToString([]byte("x:1:1")),
		base64.RawURLEncoding.EncodeToString([]byte("n:01:1")),
		base64.StdEncoding.EncodeToString([]byte("n:1:1")),
		"bjoxOjE", "bjoxOjF", "bjoxOjG", "bjoxOjH",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if number, err := ParseID(value); err == nil {
			if number < 1 || ID(number) != value {
				t.Fatalf("ParseID(%q) = %d, which is spelled %q", value, number, ID(number))
			}
		} else if err != ErrNotAnID {
			t.Fatalf("ParseID(%q): an error that is not ErrNotAnID: %v", value, err)
		}
		if receivedAt, number, err := ParseCursor(value); err == nil {
			if receivedAt < 0 || number < 1 || Cursor(receivedAt, number) != value {
				t.Fatalf("ParseCursor(%q) = (%d, %d), which is spelled %q", value, receivedAt, number, Cursor(receivedAt, number))
			}
		} else if err != ErrNotACursor {
			t.Fatalf("ParseCursor(%q): an error that is not ErrNotACursor: %v", value, err)
		}
	})
}
