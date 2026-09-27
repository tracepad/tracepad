package store

// BoundPool caps the connections the pool opens (spec 043 #16): twice the
// read slots, because a read may hold two statements at once — a listing and
// the lookups it makes per row — plus eight for everything that takes no
// slot: the writer, the background jobs, credential lookups and ingest. A
// flood of anything then waits for a connection instead of opening them
// without limit, and the headroom keeps writes and lookups from queueing
// behind reads. The connections kept between bursts stay as `idleConns` sets
// them (#24 j).
func (s *Store) BoundPool(readSlots int) {
	s.db.SetMaxOpenConns(PoolSize(readSlots))
}

// PoolSize is the most connections the pool opens for this many read slots.
func PoolSize(readSlots int) int {
	return 2*max(readSlots, 1) + 8
}
