import { ApiError } from '$lib/api/client.svelte';

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

/**
 * What to say when the screen stopped waiting for a confirmed erasure: the
 * server runs it to completion whether or not anybody waits for the answer
 * (spec 035 #14), so the clock running out is news about this screen, not a
 * failure of the erasure. `null` for any other failure, which is shown as one.
 */
export function stillRunning(cause: unknown, user: string): string | null {
	if (!(cause instanceof ApiError) || cause.details.timed_out !== true) return null;
	return (
		`The server is still erasing the data of ${user}: an erasure runs to the end even when ` +
		'this screen stops waiting. Look the user up again in a few minutes to see it finished.'
	);
}
