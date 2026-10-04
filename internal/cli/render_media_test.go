package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// A payload the server cut says when media lies beyond the cut (spec 004 #39);
// the human mode passes that on, and says nothing about media a cut did not
// leave out.
func TestATruncatedPayloadSaysWhenMediaLiesBeyondIt(t *testing.T) {
	cut := func(extra string) string {
		return payloadText(json.RawMessage(`{"truncated":true,"size":2048,"preview":"[{\"text\":\"a long` + `\"","trace_id":"t","observation_id":"o","full":"/api/v1/observations/o/io?trace_id=t"` + extra + `}`))
	}

	if got := cut(`,"media_count":2`); !strings.Contains(got, "2 media file(s) in the payload") {
		t.Errorf("payloadText = %q, want it to name the media past the cut", got)
	}
	if got := cut(""); strings.Contains(got, "media") {
		t.Errorf("payloadText = %q, want no mention of media that was not left out", got)
	}
}
