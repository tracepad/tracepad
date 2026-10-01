package ui

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestStubIconIsTheFavicon: the stub carries the logo's tile inline because
// its build has no files to serve (spec 006 #30). The tile is the owner's
// drawing, so the inline copy is held to the file the bundle serves, byte for
// byte once decoded, rather than to a second drawing that could drift.
func TestStubIconIsTheFavicon(t *testing.T) {
	match := regexp.MustCompile(`href="data:image/svg\+xml,([^"]+)"`).FindSubmatch(Stub)
	if match == nil {
		t.Fatal("the stub page carries no inline icon")
	}
	inline, err := url.PathUnescape(string(match[1]))
	if err != nil {
		t.Fatalf("the inline icon is not percent-encoded: %v", err)
	}
	favicon, err := os.ReadFile("../../ui/static/favicon.svg")
	if err != nil {
		t.Fatal(err)
	}
	if inline != strings.TrimSpace(string(favicon)) {
		t.Errorf("the stub's icon is not ui/static/favicon.svg:\n got  %s\n want %s", inline, favicon)
	}
}
