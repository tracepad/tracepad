// Package logpace lets one log line through per interval for each key, and
// counts the ones it holds back, so that a condition that repeats on every
// request — a full disk, a number JSON cannot spell — costs a line a minute
// and the line says how many it stands for.
package logpace

import (
	"sync"
	"time"
)

// Keyed paces lines per key. The zero value is ready to use once Every is set.
type Keyed struct {
	Every time.Duration

	mu   sync.Mutex
	last map[string]time.Time
	held map[string]int64
}

// Allow reports whether to log the key now, and how many lines were held back
// since the last one that was.
func (l *Keyed) Allow(key string, now time.Time) (skipped int64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last, l.held = map[string]time.Time{}, map[string]int64{}
	}
	if last, seen := l.last[key]; seen && now.Sub(last) < l.Every {
		l.held[key]++
		return 0, false
	}
	skipped = l.held[key]
	l.last[key], l.held[key] = now, 0
	return skipped, true
}
