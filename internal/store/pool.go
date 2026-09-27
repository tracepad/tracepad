package store

// BoundPool caps the connections the pool opens (spec 043 #16, #28): two for
// each read slot and for the system gauge's lane of one, because a read may
// hold two statements at once; one the writer keeps for its commits; and
// eight for everything that takes no slot — background jobs, credential
// lookups and ingest. A flood of anything then waits for a connection instead
// of opening them without limit, and the headroom keeps writes and lookups
// from queueing behind reads. The connections kept between bursts stay as
// `idleConns` sets them (#24 j).
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
