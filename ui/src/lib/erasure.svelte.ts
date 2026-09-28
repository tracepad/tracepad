import { api, ApiError, type DryRun, type Erasure } from '$lib/api/client.svelte';
import { count } from '$lib/format';

// A user-data erasure (spec 044) is a task on the server (spec 047 #6): the
// dialog asks for it to be answered within twenty seconds, which covers a user
// of a few traces, and otherwise follows it while it runs (#18). One sentence
// for its end, the same on Settings and on the user page, built from the
// server's own counts.

/** How long the dialog's confirmed request waits for the erasure to end (#18). */
export const ERASE_WAIT_SECONDS = 20;

/** How often a running erasure is read again (#18). */
export const ERASURE_POLL_MS = 2_000;

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

/** Whether an erasure is over, done or failed. */
export function ended(erasure: Pick<Erasure, 'state'>): boolean {
	return erasure.state === 'done' || erasure.state === 'failed';
}

/**
 * Where an erasure is: its state before it starts, its phase after, and in
 * the parsed phase — the one that takes the time — how far into the traces
 * step 1 counted.
 */
export function stage(erasure: Pick<Erasure, 'state' | 'phase' | 'progress'>): string {
	if (erasure.phase == null) return erasure.state;
	const whole = erasure.progress.traces_at_start;
	if (erasure.phase !== 'parsed' || whole == null) return erasure.phase;
	return `parsed, ${count(erasure.progress.traces_deleted)} of ${count(whole)} traces`;
}

/**
 * What the screen says of an erasure it follows: where it is while it runs,
 * the sentence of its end once it is done, and why when it failed.
 */
export function describe(erasure: Erasure, user: string): string {
	if (erasure.state === 'done') return erased(user, erasure.deleted);
	if (erasure.state === 'failed') {
		const before = erased(user, erasure.deleted).replace(/^Erased/, 'Before it did, it erased');
		return `The erasure of ${user}'s data failed: ${erasure.error ?? 'the server gave no reason'}. ${before}`;
	}
	return `Erasure in progress — ${stage(erasure)}`;
}

/**
 * What to say when a confirmed erasure got no answer: the request may have
 * reached the server, which records an erasure before it answers, so the
 * screen does not know whether it was accepted — and `where` says how to find
 * out (spec 047 #27). A proxy in front of the server answering 502 or 504
 * stopped waiting too. `null` for any other failure, which is shown as one:
 * the server's own refusals, a 503 among them, come before it records
 * anything.
 */
export function unanswered(cause: unknown, user: string, where: string): string | null {
	if (!(cause instanceof ApiError)) return null;
	let why: string;
	if (cause.details.timed_out === true) why = 'this screen stopped waiting';
	else if (cause.status === 502 || cause.status === 504) {
		why = `a proxy in front of the server stopped waiting (${cause.status})`;
	} else return null;
	return `No answer about erasing the data of ${user}: ${why}. The server may have accepted the erasure; ${where}.`;
}

/**
 * One erasure a screen follows: read again every two seconds until it ends.
 * Leaving the screen stops the reading and nothing else; the erasure runs on
 * the server whoever watches it.
 */
export class ErasureWatch {
	current = $state.raw<Erasure | null>(null);
	#project = '';
	#timer: ReturnType<typeof setTimeout> | undefined;
	#reading: AbortController | undefined;

	/** Whether the one followed is queued or running. */
	get running(): boolean {
		return this.current !== null && !ended(this.current);
	}

	/** Follows an erasure of a project from where it is now. */
	follow(project: string, erasure: Erasure) {
		this.stop();
		this.#project = project;
		this.current = erasure;
		if (!ended(erasure)) this.#next();
	}

	/** Stops reading, and forgets the erasure: the screen is about another user now. */
	forget() {
		this.stop();
		this.current = null;
	}

	/** Stops reading; what is on screen stays. */
	stop() {
		clearTimeout(this.#timer);
		this.#timer = undefined;
		this.#reading?.abort();
		this.#reading = undefined;
	}

	#next() {
		this.#timer = setTimeout(() => void this.#read(), ERASURE_POLL_MS);
	}

	async #read() {
		const followed = this.current;
		if (followed === null) return;
		this.#reading = new AbortController();
		const signal = this.#reading.signal;
		try {
			const read = await api.erasure(this.#project, followed.id, signal);
			// Stopped, or following another, while the answer was on its way
			// (#31): the answer is the old one's.
			if (signal.aborted) return;
			this.current = read;
		} catch (cause) {
			if (signal.aborted || (cause instanceof DOMException && cause.name === 'AbortError')) return;
			// Gone — its project purged, or its record removed — or refused,
			// its role taken away, is the end of following it, and of saying
			// it runs (#27, #31): the server would answer the same again.
			// Anything else — no answer, a 5xx — is tried again on the next
			// tick: the erasure goes on either way.
			if (cause instanceof ApiError && cause.status >= 400 && cause.status < 500) {
				this.current = null;
				return;
			}
		}
		if (this.current !== null && !ended(this.current)) this.#next();
	}
}

/**
 * A confirmed erasure's answer as the dialog shows it: the sentence of its
 * end when it ended within the wait — a failure thrown as the failure it is —
 * and otherwise that it runs on, while `watch` follows it.
 */
export function settle(
	project: string,
	user: string,
	erasure: Erasure,
	watch: ErasureWatch | null
): string {
	watch?.follow(project, erasure);
	if (erasure.state === 'failed') throw new ApiError(0, describe(erasure, user));
	if (erasure.state === 'done') return erased(user, erasure.deleted);
	if (watch === null) return `The erasure of ${user}'s data runs on the server.`;
	return (
		`The erasure of ${user}'s data runs on the server; this dialog follows it, and closing it ` +
		'stops nothing.'
	);
}

/** One confirmed erasure as a screen asks for it (#18, #27, #31). */
export interface Confirmation {
	project: string;
	user: string;
	confirm: string;
	watch: ErasureWatch;
	/** Where the erasure of a lost answer can be seen, for its sentence. */
	where: string;
	/**
	 * Whether the screen is still about this user of this project: the wait
	 * is twenty seconds, and a page is reused for the next user's id.
	 */
	still: () => boolean;
	/** What the screen does after an answer that was lost, while still. */
	lost?: () => void;
	/** What the screen does with the answer, while still, before it follows it. */
	answered?: (erasure: Erasure) => void;
}

/**
 * The confirmed request of both erase dialogs: asked with the dialog's wait,
 * a lost answer said as one, and the answer followed only by a screen still
 * about it — one that moved on is told the sentence and follows nothing, so
 * it never names its own user with another's erasure (#31).
 */
export async function confirmErasure(c: Confirmation): Promise<string> {
	let answer: DryRun | Erasure;
	try {
		answer = await api.eraseUserData(c.project, c.user, c.confirm, ERASE_WAIT_SECONDS);
	} catch (cause) {
		const sentence = unanswered(cause, c.user, c.where);
		if (sentence === null) throw cause;
		if (c.still()) c.lost?.();
		throw new ApiError(0, sentence);
	}
	const erasure = answer as Erasure;
	if (!c.still()) return settle(c.project, c.user, erasure, null);
	c.answered?.(erasure);
	return settle(c.project, c.user, erasure, c.watch);
}
