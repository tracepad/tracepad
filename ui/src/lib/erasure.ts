import { ApiError, REQUEST_TIMEOUT_MS } from '$lib/api/client.svelte';

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
 * server runs one it received to completion whether or not anybody waits for
 * the answer (spec 035 #14), so the clock running out is news about this
 * screen, not a failure of the erasure. So is a proxy in front of the server
 * answering 502 or 504 because it stopped waiting too; a 503 is the server's
 * own, sent before it erases anything. A browser cannot tell whether the
 * request reached the server, so the sentence does not claim that it did.
 * `null` for any other failure, which is shown as one.
 */
export function stillRunning(cause: unknown, user: string): string | null {
	if (!(cause instanceof ApiError)) return null;
	let why: string;
	if (cause.details.timed_out === true) {
		why = `this screen stopped waiting after ${REQUEST_TIMEOUT_MS / 1000} seconds`;
	} else if (cause.status === 502 || cause.status === 504) {
		why = `a proxy in front of the server stopped waiting (${cause.status})`;
	} else {
		return null;
	}
	return (
		`No answer about erasing the data of ${user}: ${why}. An erasure the server received ` +
		'runs to the end without anybody waiting: look the user up again in a few minutes to ' +
		'see what is left.'
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
