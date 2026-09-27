package logpace

import (
	"testing"
	"time"
)

func TestOneLinePerIntervalPerKey(t *testing.T) {
	l := &Keyed{Every: time.Minute}
	start := time.Unix(0, 0)
	if _, ok := l.Allow("a", start); !ok {
		t.Fatal("the first line was held")
	}
	for range 3 {
		if _, ok := l.Allow("a", start.Add(time.Second)); ok {
			t.Fatal("a second line inside the minute went through")
		}
	}
	if _, ok := l.Allow("b", start.Add(time.Second)); !ok {
		t.Error("another key was held by the first")
	}
	skipped, ok := l.Allow("a", start.Add(time.Minute))
	if !ok || skipped != 3 {
		t.Errorf("after the minute = %d, %v, want the line through saying it stands for 3", skipped, ok)
	}
}
