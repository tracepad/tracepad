// What a confirmed user-data erasure says it did (spec 044): one sentence, the
// same on Settings and on the user page, built from the server's own counts.

/**
 * The traces, and what the raw archive lost: the user's spans taken out of
 * the batches that held them, which is the part of an erasure nothing else on
 * screen shows.
 */
export function erased(user: string, deleted: Record<string, number>): string {
	const traces = deleted.traces ?? 0;
	const spans = deleted.raw_spans ?? 0;
	const batches = (deleted.raw_batches_rewritten ?? 0) + (deleted.raw_batches_deleted ?? 0);
	let sentence = `Erased ${traces} ${traces === 1 ? 'trace' : 'traces'} belonging to ${user}`;
	if (spans > 0) {
		sentence += `, and ${spans} ${spans === 1 ? 'span' : 'spans'} from ${batches} raw ${
			batches === 1 ? 'batch' : 'batches'
		}`;
	}
	return `${sentence}.`;
}
