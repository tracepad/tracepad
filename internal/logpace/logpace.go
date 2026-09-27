// Package logpace lets one log line through per interval for each key, and
// counts the ones it holds back, so that a condition that repeats on every
// request — a full disk, a number JSON cannot spell, a refused bomb — costs a
// line a minute and the line says how many it stands for.
package logpace

import (
	"sync"
	"time"
)

// Keyed paces lines per key: one sender repeating itself cannot hold the line
// another needs, and each line counts only its own key's repeats. The zero
// value is ready to use once Every is set.
//
// Keys caps how many keys an interval admits, for a warning whose key the
// sender chooses: a sender inventing a new key per request cannot fill the log
// either, and what the cap turned away is counted apart and told with the next
// line let through. Zero admits any number, for keys the code chooses — a
// condition, a field, a route.
type Keyed struct {
	Every time.Duration
	Keys  int

	mu      sync.Mutex
	seen    map[string]*key
	overCap int64
}

type key struct {
	at      time.Time
	skipped int64
}

// Held is what a line let through stands for: its own key's repeats since its
// last line, and the lines of other keys the cap turned away meanwhile.
type Held struct {
	SameKey, OverCap int64
}

// Allow reports whether to log the key now, and what the line stands for.
func (l *Keyed) Allow(name string, now time.Time) (Held, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]*key{}
	}
	k, known := l.seen[name]
	if known && now.Sub(k.at) < l.Every {
		k.skipped++
		return Held{}, false
	}
	// The cap counts the keys logged within the interval, and a key coming
	// back after its own interval is held to it like a new one: otherwise
	// yesterday's keys, returning, would double what a minute may log.
	active := 0
	for other, o := range l.seen {
		switch {
		case now.Sub(o.at) < l.Every:
			active++
		case other != name && (o.skipped == 0 || l.Keys > 0 && len(l.seen) > 2*l.Keys):
			// Nothing to tell, or kept long enough: a key that comes
			// back after this starts its count again.
			delete(l.seen, other)
		}
	}
	if l.Keys > 0 && active >= l.Keys {
		l.overCap++
		return Held{}, false
	}
	held := Held{OverCap: l.overCap}
	if known {
		held.SameKey = k.skipped
		k.at, k.skipped = now, 0
	} else {
		l.seen[name] = &key{at: now}
	}
	l.overCap = 0
	return held, true
}
