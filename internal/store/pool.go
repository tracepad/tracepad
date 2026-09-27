package store

// BoundPool caps the connections the pool opens (spec 043 #16, #28, #29).
// Every read — a read route's, or one a write route makes in a read slot —
// holds one statement at a time, so a slot needs one connection; the pool
// opens two for each read slot and for the system gauge's lane of one, the
// second an equal share for the work that takes no slot and does not wait
// for one: credential lookups, ingest's lookups, the background jobs. On top
// of that, one the writer keeps for its commits, and eight more. A flood of
// anything then waits for a connection instead of opening them without
// limit, and reads alone never take more than half of the pool. The
// connections kept between bursts stay as `idleConns` sets them (#24 j).
func (s *Store) BoundPool(readSlots int) {
	s.db.SetMaxOpenConns(PoolSize(readSlots))
}

// PoolSize is the most connections the pool opens for this many read slots.
func PoolSize(readSlots int) int {
	const (
		systemLane = 1
		writer     = 1
		headroom   = 8
	)
	return 2*(max(readSlots, 1)+systemLane) + writer + headroom
}
