package logpace

import (
	"fmt"
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
	held, ok := l.Allow("a", start.Add(time.Minute))
	if !ok || held.SameKey != 3 {
		t.Errorf("after the minute = %d, %v, want the line through saying it stands for 3", held.SameKey, ok)
	}
}

func TestPacesAndCounts(t *testing.T) {
	l := &Keyed{Every: time.Minute}
	start := time.Unix(1_000_000, 0)
	if held, ok := l.Allow("", start); !ok || held.SameKey != 0 {
		t.Fatalf("first = (%d, %v), want (0, true)", held.SameKey, ok)
	}
	for i := range 3 {
		if _, ok := l.Allow("", start.Add(time.Duration(i+1)*time.Second)); ok {
			t.Fatalf("call %d inside the minute was let through", i+2)
		}
	}
	if held, ok := l.Allow("", start.Add(time.Minute)); !ok || held.SameKey != 3 {
		t.Fatalf("a minute later = (%d, %v), want (3, true)", held.SameKey, ok)
	}
}

// TestByKey: each key once per interval, at most Keys of them at a time; a
// line let through counts only its own key's repeats, and what the cap turned
// away is told apart, once, with the next line.
func TestByKey(t *testing.T) {
	l := &Keyed{Every: time.Minute, Keys: 2}
	start := time.Unix(1_700_000_000, 0)
	at := func(key string, offset time.Duration) (Held, bool) {
		return l.Allow(key, start.Add(offset))
	}
	expect := func(name string, got Held, ok bool, wantOK bool, want Held) {
		t.Helper()
		if ok != wantOK || got != want {
			t.Errorf("%s: ok = %v, held = %+v; want ok = %v, held = %+v", name, ok, got, wantOK, want)
		}
	}
	held, ok := at("a", 0)
	expect("a, first", held, ok, true, Held{})
	held, ok = at("a", time.Second)
	expect("a, repeated", held, ok, false, Held{})
	held, ok = at("a", 2*time.Second)
	expect("a, repeated again", held, ok, false, Held{})
	held, ok = at("b", 3*time.Second)
	expect("b, first: a's repeats are not b's", held, ok, true, Held{})
	held, ok = at("b", 4*time.Second)
	expect("b, repeated", held, ok, false, Held{})
	held, ok = at("c", 5*time.Second)
	expect("c, past the cap", held, ok, false, Held{})
	held, ok = at("a", time.Minute)
	expect("a, next interval", held, ok, true, Held{SameKey: 2, OverCap: 1})
	held, ok = at("b", time.Minute+3*time.Second)
	expect("b, next interval", held, ok, true, Held{SameKey: 1})
	held, ok = at("c", time.Minute+4*time.Second)
	expect("c, still past the cap", held, ok, false, Held{})
	held, ok = at("c", 2*time.Minute+5*time.Second)
	expect("c, once there is room", held, ok, true, Held{OverCap: 1})
}

// TestReturningKeysAreHeldToTheCap: keys whose interval has passed are held to
// the same cap as new ones when they come back, so an interval never logs
// more than Keys lines — however the keys are shuffled between intervals.
func TestReturningKeysAreHeldToTheCap(t *testing.T) {
	const keys = 64
	l := &Keyed{Every: time.Minute, Keys: keys}
	start := time.Unix(1_700_000_000, 0)
	key := func(batch, i int) string { return fmt.Sprintf("https://%d-%d.example", batch, i) }
	logged := func(from time.Time, batches ...int) int {
		n := 0
		for _, batch := range batches {
			for i := range keys {
				if _, ok := l.Allow(key(batch, i), from); ok {
					n++
				}
				// Each key once more within its interval, so it is kept
				// with something to tell.
				l.Allow(key(batch, i), from)
			}
		}
		return n
	}
	if n := logged(start, 0); n != keys {
		t.Fatalf("the first interval logged %d, want %d", n, keys)
	}
	// The next interval: 64 new keys, then the first 64 back again.
	if n := logged(start.Add(time.Minute), 1, 0); n != keys {
		t.Errorf("an interval of new keys and returning ones logged %d lines, want at most %d", n, keys)
	}
}

// TestUncappedKeysAreNotTurnedAway: without Keys every key gets its line, and
// a key's repeats are kept for its next line however many other keys come
// and go meanwhile.
func TestUncappedKeysAreNotTurnedAway(t *testing.T) {
	l := &Keyed{Every: time.Minute}
	start := time.Unix(0, 0)
	l.Allow("kept", start)
	l.Allow("kept", start.Add(time.Second))
	for i := range 200 {
		if _, ok := l.Allow(fmt.Sprint(i), start.Add(2*time.Minute)); !ok {
			t.Fatalf("key %d was turned away with no cap", i)
		}
	}
	if held, ok := l.Allow("kept", start.Add(3*time.Minute)); !ok || held != (Held{SameKey: 1}) {
		t.Errorf("the kept key = %+v, %v; want its one repeat told", held, ok)
	}
}
