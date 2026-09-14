import { flushSync } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from './api/client.svelte';
import { Listing, StateSpot, Walk, type Answer } from './listing.svelte';
import type { PageState } from './page';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({ page: { url: new URL('http://tracepad.test/traces') } }));

// The loader's five invariants, over a fake reader under `$effect.root` — which
// is what a module of effects buys over a component (spec 010 #1). Each of them
// held only in one of the two pages at some point in spec 009's review, and
// this is where that stops being a thing somebody has to remember.

type Row = { id: string; key: string };

/** A page of rows, newest first, keyed the way the server orders them. */
const answer = (ids: string[], extra: Partial<Answer<Row>> = {}): Answer<Row> => ({
	rows: ids.map((id) => ({ id, key: `2026-08-30T00:00:0${id}Z` })),
	next_cursor: null,
	prev_cursor: null,
	...extra
});

/**
 * What the client throws when a request goes unanswered for thirty seconds
 * (spec 010 #10): a failure with the server's silence as its message, and not
 * an abort — an abort lands nowhere, and this has to land somewhere.
 */
const unanswered = () => new ApiError(0, 'the server did not answer in time');

type Call = {
	at: PageState;
	count: boolean;
	signal: AbortSignal;
	ok: (page: Answer<Row>) => void;
	no: (cause: unknown) => void;
};

/** A reader that records what it was asked and answers when a test says so. */
function reader() {
	const asked: Call[] = [];
	return {
		asked,
		get loads() {
			return asked.filter((call) => !call.count);
		},
		get counts() {
			return asked.filter((call) => call.count);
		},
		read: (at: PageState, count: boolean, signal: AbortSignal) =>
			new Promise<Answer<Row>>((ok, no) => asked.push({ at, count, signal, ok, no }))
	};
}

/** Lets an answer reach the loader, and the loader's writes reach the effects. */
async function idle() {
	await new Promise((resolve) => setTimeout(resolve, 0));
	flushSync();
}

// What a page reads and the loader does not page by: the filters' object
// identity, a `?peek=`, an `?obs=`. The fake reader touches it on every call, so
// a `read` that were not `untrack`ed would subscribe the load effect to it.
let noise = $state('');
let which = $state('a');
let scope = $state('one');
let peeked = $state<string | null>(null);
let stop = () => {};

beforeEach(() => {
	noise = '';
	which = 'a';
	scope = 'one';
	peeked = null;
});
afterEach(() => stop());

/** A listing and its walk, driven the way a component would drive them. */
function mount(count = false, ascending = false, rows?: () => Row[]) {
	const source = reader();
	const opened: string[] = [];
	let listing!: Listing<Row>;
	let walk!: Walk<Row>;
	stop = $effect.root(() => {
		listing = new Listing<Row>({
			key: () => which,
			spot: new StateSpot(() => scope),
			count,
			read: (at, counting, signal) => {
				void noise;
				return source.read(at, counting, signal);
			},
			failed: 'the listing failed'
		});
		walk = new Walk(listing, {
			key: (row) => row.key,
			peekID: () => peeked,
			showing: () => null,
			open: (id) => {
				peeked = id;
				opened.push(id);
			},
			ascending,
			rows
		});
	});
	flushSync();
	return { source, listing, walk, opened };
}

describe('the load', () => {
	it('re-runs for the key and the page, and for nothing else', () => {
		const { source, listing } = mount();
		expect(source.loads).toHaveLength(1);

		// Opening the panel is a `?peek=` on this same URL (PR #10 review).
		noise = 'peek';
		flushSync();
		expect(source.loads).toHaveLength(1);

		listing.turn({ cursor: 'c2' });
		flushSync();
		which = 'b';
		flushSync();
		expect(source.loads).toHaveLength(3);
	});

	it('aborts the request it interrupts, and an aborted answer lands nothing', async () => {
		const { source, listing } = mount();
		listing.turn({ cursor: 'c2' });
		flushSync();

		expect(source.loads).toHaveLength(2);
		expect(source.loads[0].signal.aborted).toBe(true);
		source.loads[0].ok(answer(['1'], { next_cursor: 'stale' }));
		await idle();
		expect(listing.rows).toEqual([]);
		expect(listing.nextCursor).toBeNull();
		// Not its failure either, and not the end of the loading it interrupted.
		source.loads[0].no(new Error('too late'));
		await idle();
		expect(listing.failure).toBeNull();
		expect(listing.loading).toBe(true);
	});

	it('empties a page that failed and leaves one that was aborted', async () => {
		const { source, listing } = mount();
		source.loads[0].ok(answer(['2', '1'], { next_cursor: 'c2', prev_cursor: 'c0' }));
		await idle();

		listing.turn({ cursor: 'c2' });
		flushSync();
		expect(listing.rows).toHaveLength(2);
		expect(listing.nextCursor).toBe('c2');

		source.loads[1].no(new Error('gone'));
		await idle();
		expect(listing.rows).toEqual([]);
		expect(listing.nextCursor).toBeNull();
		expect(listing.prevCursor).toBeNull();
		expect(listing.failure).toBe('the listing failed');
	});

	it('lowers the spinner on a load the clock gave up on, and a tick goes out after it', async () => {
		const { source, listing } = mount();
		expect(listing.loading).toBe(true);

		// Unanswered for thirty seconds, the request is failed by the client
		// rather than aborted (spec 010 #10), so it lands as a failure would.
		source.loads[0].no(unanswered());
		await idle();
		expect(listing.loading).toBe(false);
		expect(listing.failure).toBe('the server did not answer in time');
		expect(listing.rows).toEqual([]);

		// With `loading` down, the gate opens and live carries on (#9).
		void listing.tick();
		expect(source.counts).toHaveLength(1);
		source.counts[0].ok(answer(['1'], { total: 1 }));
		await idle();
		expect(listing.rows.map((row) => row.id)).toEqual(['1']);
	});
});

describe('the count', () => {
	it('is a question about the key, not about the page', async () => {
		const { source, listing } = mount(true);
		expect(source.counts).toHaveLength(1);
		expect(source.counts[0].at.limit).toBe(1);

		listing.turn({ cursor: 'c2' });
		flushSync();
		expect(source.counts).toHaveLength(1);

		which = 'b';
		flushSync();
		expect(source.counts).toHaveLength(2);
		source.counts[1].no(new Error('too expensive'));
		await idle();
		// A number the bar leaves out, over a listing that arrived perfectly well.
		expect(listing.total).toBeNull();
		expect(listing.failure).toBeNull();
	});

	it('is asked again by a reload, along with the page', () => {
		const { source, listing } = mount(true);
		source.loads[0].ok(answer(['1']));

		listing.reload();
		flushSync();
		expect(source.loads).toHaveLength(2);
		expect(source.counts).toHaveLength(2);
		expect(listing.loading).toBe(true);
	});
});

describe('a live tick', () => {
	it('replaces the rows, refreshes the total, and raises no loading state', async () => {
		const { source, listing } = mount();
		source.loads[0].ok(answer(['1'], { next_cursor: 'c2' }));
		await idle();

		void listing.tick();
		source.counts[0].ok(answer(['2', '1'], { next_cursor: 'c3', total: 9 }));
		await idle();
		expect(listing.rows.map((row) => row.id)).toEqual(['2', '1']);
		expect(listing.total).toEqual({ value: 9, capped: false });
		expect(listing.loading).toBe(false);
	});

	it('runs one at a time, so a late answer cannot land over a newer one', async () => {
		const { source, listing } = mount();
		source.loads[0].ok(answer(['1'], { next_cursor: 'c2' }));
		await idle();

		// The interval fires again while the first tick is still out (#9).
		void listing.tick();
		void listing.tick();
		expect(source.counts).toHaveLength(1);

		source.counts[0].ok(answer(['2', '1'], { next_cursor: 'c3', total: 9 }));
		await idle();
		expect(listing.rows.map((row) => row.id)).toEqual(['2', '1']);

		// And the one after it goes out as usual.
		void listing.tick();
		expect(source.counts).toHaveLength(2);
	});

	it('takes the total from under a count issued before it', async () => {
		const { source, listing } = mount(true);
		source.loads[0].ok(answer(['1'], { next_cursor: 'c2' }));
		await idle();

		// The count taken at the key change is still out — it is the one read
		// the load's controller does not hold — when the tick lands its own,
		// later number (#9).
		void listing.tick();
		source.counts[1].ok(answer(['2', '1'], { next_cursor: 'c3', total: 9 }));
		await idle();
		expect(listing.total).toEqual({ value: 9, capped: false });

		expect(source.counts[0].signal.aborted).toBe(true);
		source.counts[0].ok(answer(['1'], { total: 3 }));
		await idle();
		expect(listing.total).toEqual({ value: 9, capped: false });
	});

	it('does not go out beside a load, which would land over it', async () => {
		const { source, listing } = mount();

		// The poll timer keeps its phase across a filter change — the page's
		// effect depends on `live` and `newest`, not on the load — so a tick can
		// fire while the load for the newest page is still out. Its answer would
		// be the newer one, and the load's would land last (#9).
		void listing.tick();
		expect(source.asked).toHaveLength(1);

		source.loads[0].ok(answer(['2', '1'], { next_cursor: 'c2' }));
		await idle();
		expect(listing.rows.map((row) => row.id)).toEqual(['2', '1']);

		// And once the page has landed, live carries on.
		void listing.tick();
		expect(source.counts).toHaveLength(1);
	});

	it('is freed again by the turn that aborts it', async () => {
		const { source, listing } = mount();
		source.loads[0].ok(answer(['1'], { next_cursor: 'c2' }));
		await idle();

		void listing.tick();
		listing.turn({ cursor: 'c2' });
		flushSync();
		expect(source.counts[0].signal.aborted).toBe(true);
		source.counts[0].ok(answer(['9']));
		await idle();
		expect(listing.rows.map((row) => row.id)).toEqual(['1']);

		listing.turn({});
		flushSync();
		source.loads[2].ok(answer(['2', '1'], { next_cursor: 'c3' }));
		await idle();
		void listing.tick();
		expect(source.counts).toHaveLength(2);
	});

	it('is freed by the clock when the server never answers, and says so in its own slot', async () => {
		const { source, listing } = mount();
		source.loads[0].ok(answer(['1'], { next_cursor: 'c2' }));
		await idle();

		// A tick that never settled held the gate for ever with `liveFailure`
		// still `null` — a silent freeze the toggle could not undo. The client
		// now fails it after thirty seconds (spec 010 #10), which reaches this
		// `catch` as any failure does and frees the gate in the `finally`.
		void listing.tick();
		source.counts[0].no(unanswered());
		await idle();
		expect(listing.liveFailure).toBe('the server did not answer in time');
		// The rows already on screen are kept, and no loading state was raised.
		expect(listing.rows.map((row) => row.id)).toEqual(['1']);
		expect(listing.failure).toBeNull();
		expect(listing.loading).toBe(false);

		// The next tick goes out, and its answer clears the slot.
		void listing.tick();
		expect(source.counts).toHaveLength(2);
		source.counts[1].ok(answer(['2', '1'], { next_cursor: 'c3', total: 2 }));
		await idle();
		expect(listing.liveFailure).toBeNull();
		expect(listing.rows.map((row) => row.id)).toEqual(['2', '1']);
	});

	it('does nothing off the newest page', async () => {
		const { source, listing } = mount();
		source.loads[0].ok(answer(['1'], { next_cursor: 'c2' }));
		await idle();
		listing.turn({ cursor: 'c2' });
		flushSync();

		await listing.tick();
		expect(source.asked).toHaveLength(2);
	});
});

describe('the page in component state', () => {
	it('starts a new scope at its first page, with no write and no second request', () => {
		const { source, listing } = mount();
		listing.turn({ limit: 25, cursor: 'c2' });
		flushSync();
		expect(source.loads).toHaveLength(2);

		scope = 'two';
		flushSync();
		expect(source.loads).toHaveLength(3);
		// The size is a preference and travels; the cursor is a position and does not.
		expect(source.loads[2].at).toEqual({ limit: 25, cursor: null, direction: 'next' });
	});
});

describe('a walk that ran out of page', () => {
	it('settles on the page it asked for, and loses its intent to a failure', async () => {
		const { source, listing, walk, opened } = mount();
		source.loads[0].ok(answer(['3', '2'], { next_cursor: 'c2' }));
		await idle();
		peeked = '2';
		flushSync();

		// No row older than '2' on this page, so the walk turns it (spec 009 #6).
		walk.step(1);
		flushSync();
		expect(source.loads[1].at).toEqual({ limit: 50, cursor: 'c2', direction: 'next' });
		source.loads[1].ok(answer(['1', '0'], { next_cursor: 'c3' }));
		await idle();
		expect(opened).toEqual(['1']);

		peeked = '0';
		flushSync();
		walk.step(1);
		flushSync();
		source.loads[2].no(new Error('gone'));
		await idle();
		// A page that failed cannot be landed on, and neither can the walk.
		expect(opened).toEqual(['1']);
		expect(listing.rows).toEqual([]);
	});

	// A page that draws fewer rows than the loader landed — the comparison with
	// its unchanged rows hidden (spec 016 #10) — is walked as it is drawn: a
	// step must not open a row the reader cannot see.
	it('walks the rows the page draws, not the ones the loader landed', async () => {
		const { source, listing, walk, opened } = mount(false, false, () =>
			listing.rows.filter((row) => row.id !== '2')
		);
		source.loads[0].ok(answer(['3', '2', '1']));
		await idle();
		peeked = '3';
		flushSync();

		walk.step(1);
		flushSync();
		expect(opened).toEqual(['1']);
		// And the hidden row is not counted at the end of the listing either.
		peeked = '1';
		flushSync();
		expect(walk.hasNext).toBe(false);
	});

	// A listing read oldest first — a dataset's items, in `seq` order — walks
	// down the page as it is drawn, and its `next` cursor is still the page
	// after this one (spec 016 #3).
	it('walks an ascending listing down the page as drawn', async () => {
		const { source, walk, opened } = mount(false, true);
		source.loads[0].ok(answer(['1', '2', '3'], { next_cursor: 'c2' }));
		await idle();
		peeked = '2';
		flushSync();
		expect(walk.hasPrev).toBe(true);
		expect(walk.hasNext).toBe(true);

		walk.step(1);
		flushSync();
		expect(opened).toEqual(['3']);
		walk.step(-1);
		flushSync();
		expect(opened).toEqual(['3', '2']);

		peeked = '3';
		flushSync();
		walk.step(1);
		flushSync();
		expect(source.loads[1].at).toEqual({ limit: 50, cursor: 'c2', direction: 'next' });
		source.loads[1].ok(answer(['4', '5'], { prev_cursor: 'c1' }));
		await idle();
		expect(opened).toEqual(['3', '2', '4']);
	});
});
