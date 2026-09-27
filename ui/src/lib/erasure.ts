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

/**
 * A screen's erasure requests, and whether its last confirmed one was left
 * running on the server — which is when the screen stays where it is rather
 * than leave as if the user were gone. Per attempt: a retry that answers
 * clears it, so a screen that stayed for one running erasure still leaves
 * after the one that finished.
 */
export class Erasure {
	running = false;

	/**
	 * The call's answer; for a confirmed erasure the screen stopped waiting
	 * for, the sentence saying it is still running.
	 */
	async ask<T>(user: string, confirm: string | undefined, call: () => Promise<T>): Promise<T | string> {
		if (confirm !== undefined) this.running = false;
		try {
			return await call();
		} catch (cause) {
			const sentence = confirm === undefined ? null : stillRunning(cause, user);
			if (sentence === null) throw cause;
			this.running = true;
			return sentence;
		}
	}
}
