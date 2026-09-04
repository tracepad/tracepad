// The part three listings *are* (spec 010): the page in force, the rows on it,
// its two cursors, the capped count, and the effects that keep them true. Held
// once, because the three copies drifted in pairs — five defects spec 009's
// review found were the same defect in the neighbouring file.
//
// A runes module rather than a component (#1): what is shared is reactive, and
// a component could hand that back only through props and snippets — the same
// wiring in a new shape. An `$effect` created by a constructor called during
// setup belongs to the component that called it and is torn down with it;
// under `$effect.root`, it belongs to a test.

import { untrack } from 'svelte';
import { goto } from '$app/navigation';
import { page } from '$app/state';
import { ApiError, type Page } from '$lib/api/client.svelte';
import {
	DEFAULT_PAGE_SIZE,
	isFirstPage,
	isLastPage,
	pageSearch,
	readPage,
	type PageState
} from '$lib/page';
import { anchor, neighbour, settled, walkable, type Ordered, type Rolling } from '$lib/peek';

/** Where the page in force lives, and how it moves (#2). */
export type Spot = {
	readonly at: PageState;
	/** Keys left out of `to` reset, which is what `pageSearch` means. */
	turn(to: Partial<PageState>): void;
};

/** The page in the URL, so that a listing is reload-safe (spec 009 #1). */
export class UrlSpot implements Spot {
	at = $derived(readPage(page.url.searchParams));

	turn(to: Partial<PageState>) {
		const search = pageSearch(page.url.searchParams, { limit: this.at.limit, ...to });
		goto(`${page.url.pathname}${search}`, { keepFocus: true, noScroll: true });
	}
}

/**
 * The page in component state, which is what a session's own trace table has to
 * do (spec 009 #10): it renders over a URL whose `limit` and `cursor` already
 * belong to the listing behind it. Paired with the scope it was taken in and
 * derived rather than reset by an effect: a cursor belongs to its scope, so one
 * naming another *is not* the page — no write, no second render, no second
 * request.
 */
export class StateSpot implements Spot {
	// Asserted, here and in the two classes below: a `$derived` field is a lazy
	// thunk and may read what the constructor sets, which TypeScript — seeing a
	// field initializer — reads as a use before assignment.
	#scope!: () => string;
	#chosen = $state.raw<{ scope: string | null; at: PageState }>({
		scope: null,
		at: { limit: DEFAULT_PAGE_SIZE, cursor: null, direction: 'next' }
	});
	at = $derived<PageState>(
		this.#chosen.scope === this.#scope()
			? this.#chosen.at
			: { ...this.#chosen.at, cursor: null, direction: 'next' }
	);

	constructor(scope: () => string) {
		this.#scope = scope;
	}

	turn(to: Partial<PageState>) {
		const at = { limit: this.at.limit, cursor: null, direction: 'next' as const, ...to };
		this.#chosen = { scope: this.#scope(), at };
	}
}

export type Total = { value: number; capped: boolean };

/** A page of any of the three listings, as the loader reads it. */
export type Answer<Row> = {
	rows: Row[];
	next_cursor: string | null;
	prev_cursor: string | null;
	total?: number;
	total_capped?: boolean;
};

export type Spec<Row> = {
	/** Everything that names *which* listing: the filters, or the scope. */
	key: () => string;
	spot: Spot;
	read: (at: PageState, count: boolean, signal: AbortSignal) => Promise<Answer<Row>>;
	/** Whether a capped count is asked for at all (divergence 6). */
	count?: boolean;
	/** What to say when a failure is not the server's own words. */
	failed: string;
};

/** A `PageState` as the read API's page parameters. */
export function asPage(at: PageState, count: boolean): Page {
	return { limit: at.limit, cursor: at.cursor ?? undefined, direction: at.direction, count };
}

/** A count asks for one row, because what it wants is the number beside it. */
const COUNT_ONLY: PageState = { limit: 1, cursor: null, direction: 'next' };

function counted<Row>(answer: Answer<Row>): Total | null {
	return answer.total === undefined
		? null
		: { value: answer.total, capped: answer.total_capped ?? false };
}

export class Listing<Row> {
	rows = $state.raw<Row[]>([]);
	nextCursor = $state.raw<string | null>(null);
	prevCursor = $state.raw<string | null>(null);
	total = $state.raw<Total | null>(null);
	loading = $state(true);
	failure = $state<string | null>(null);
	/**
	 * `tick()`'s own slot: a tick that recovers must not erase a failure the
	 * reader still needs, and a failed tick must not masquerade as a failure of
	 * the page on screen. The banner shows `problem` (divergence 3).
	 */
	liveFailure = $state<string | null>(null);

	#spec!: Spec<Row>;
	/** Bumped by `reload()`, which is a page turn that lands where it started. */
	#again = $state(0);
	/** How a landed — or failed — page reaches the walk over it, if there is one. */
	#land: ((at: PageState | null) => void) | null = null;
	/**
	 * Every request belongs to one page of one key, and changing either aborts
	 * the lot: a tick still in flight would answer the previous question.
	 */
	#query: AbortController | null = null;
	/**
	 * Whether a tick is still out. Live fires on a timer, so a server slower
	 * than the interval had two ticks in flight at once and the older one could
	 * land last, rolling the rows, the cursor and the count back a whole
	 * interval (#9). The other half of that gate is `loading`, read in `tick()`:
	 * a tick can overlap a *load* the same way.
	 */
	#ticking = false;
	/**
	 * The count still in flight, if there is one. It is the one read the load's
	 * controller does not cover — its own effect owns it — so a count taken at
	 * the last key change could answer after a tick's fresher number and roll
	 * `total` back (#9). A tick that lands a total abandons it.
	 */
	#counting: AbortController | null = null;

	#at = $derived(this.#spec.spot.at);
	/**
	 * The subscription, as values that compare. Not the objects behind them:
	 * both are rebuilt on every URL change and a `$derived` object is never
	 * equal to the last one, so opening the panel — a `?peek=` on this same URL
	 * — re-ran the load and threw away the page the reader was on (PR #10).
	 */
	#which = $derived(this.#spec.key());
	#stamp = $derived(
		`${this.#which}|${this.#at.limit}|${this.#at.direction}|${this.#at.cursor ?? ''}`
	);

	newest = $derived(isFirstPage(this.#at));
	oldest = $derived(isLastPage(this.#at));
	/** What the banner says: the load's failure, or a tick's (divergence 3). */
	problem = $derived(this.failure ?? this.liveFailure);
	/** The bar's props as one object (#4), spread at the call site with the noun. */
	bar = $derived({
		limit: this.#at.limit,
		rows: this.rows.length,
		total: this.total,
		hasPrev: this.prevCursor !== null,
		hasNext: this.nextCursor !== null,
		busy: this.loading,
		atNewest: this.newest,
		atOldest: this.oldest,
		onresize: (limit: number) => this.turn({ limit }),
		onfirst: () => this.turn({}),
		onprev: () => this.turn({ cursor: this.prevCursor ?? undefined, direction: 'prev' }),
		onnext: () => this.turn({ cursor: this.nextCursor ?? undefined }),
		onlast: () => this.turn({ direction: 'prev' })
	});

	constructor(spec: Spec<Row>) {
		this.#spec = spec;
		$effect(() => {
			// The stamp is the whole subscription; the page is handed down
			// untracked, because `read` uses it before its first `await` — in
			// this effect's own run — and reading it there would subscribe.
			void this.#stamp;
			void this.#again;
			const controller = new AbortController();
			this.#query = controller;
			void this.#load(untrack(() => this.#at), controller.signal);
			return () => controller.abort();
		});
		if (spec.count === false) return;
		$effect(() => {
			// The count is a question about the key and not about the page
			// (spec 009 #4): asked when the key moves, and never on a turn.
			void this.#which;
			void this.#again;
			const controller = new AbortController();
			this.#counting = controller;
			void this.#count(controller.signal);
			return () => controller.abort();
		});
	}

	async #load(wanted: PageState, signal: AbortSignal) {
		this.loading = true;
		this.failure = this.liveFailure = null;
		try {
			const answer = await untrack(() => this.#spec.read(wanted, false, signal));
			if (signal.aborted) return;
			this.rows = answer.rows;
			this.nextCursor = answer.next_cursor;
			this.prevCursor = answer.prev_cursor;
			// After the rows, because settling a walk reads the page that landed.
			this.#land?.(wanted);
		} catch (cause) {
			if (signal.aborted) return;
			this.rows = [];
			this.nextCursor = this.prevCursor = null;
			// A page that failed cannot be landed on, and a walk waiting for it
			// has nowhere to go. An *aborted* one is another story — the turn
			// aborts what it interrupted — so that intent survives it and is
			// settled by matching cursors (spec 009 #6).
			this.#land?.(null);
			this.failure = this.#describe(cause);
		} finally {
			if (!signal.aborted) this.loading = false;
		}
	}

	async #count(signal: AbortSignal) {
		try {
			const answer = await untrack(() => this.#spec.read(COUNT_ONLY, true, signal));
			if (!signal.aborted) this.total = counted(answer);
		} catch {
			// A count nobody can produce is a number the bar leaves out, not an
			// error over a listing that arrived perfectly well.
			if (!signal.aborted) this.total = null;
		}
	}

	#describe(cause: unknown): string {
		return cause instanceof ApiError ? cause.message : this.#spec.failed;
	}

	turn(to: Partial<PageState>) {
		this.#spec.spot.turn(to);
	}

	/** Decision 5: a re-read that turns the page it is already on. */
	reload() {
		this.#again++;
	}

	/** One live tick (Decision 5): the newest page again, silently. */
	async tick() {
		const controller = this.#query;
		// Live means "the newest page, again" (spec 009 #7); anywhere else there
		// is nothing for a tick to mean.
		if (!controller || !this.newest) return;
		// One read of this page at a time (#9). Skipped rather than raced: the
		// answer still out is the newer question's answer too, and cancelling it
		// for a fresh request would leave a server slower than the interval
		// refreshing nothing at all, every tick aborted by the next.
		//
		// A load counts, not just another tick. The timer's phase survives a
		// filter change — the page's effect depends on `live` and `newest`, not
		// on the load — so a tick fired at t=5 could answer before the load
		// issued at t=0 and be overwritten by it. `loading` is always cleared by
		// a load that settles unaborted, and an aborted one is replaced by the
		// load that aborted it, so this cannot wedge.
		if (this.#ticking || this.loading) return;
		this.#ticking = true;
		const { signal } = controller;
		try {
			// Counted on the way past: live streams rows in, and a total taken
			// when the key last moved would go stale for the life of the URL
			// (PR #11 review). The count is capped, so it costs nothing a tick
			// was not already paying.
			const answer = await untrack(() => this.#spec.read(this.#at, true, signal));
			if (signal.aborted) return;
			// Replaced rather than merged: with a window anchored at "newest" the
			// page just fetched *is* the window (spec 009 #9).
			this.rows = answer.rows;
			this.nextCursor = answer.next_cursor;
			if (answer.total !== undefined) {
				// This number was taken later than any count still out, which is
				// therefore stale the moment it lands — the same rule as the rows,
				// over the one read the load's controller never held (#9).
				this.#counting?.abort();
				this.total = counted(answer);
			}
			this.liveFailure = null;
		} catch (cause) {
			if (signal.aborted) return;
			// A server that went away mid-tick is worth saying once, but not
			// worth throwing away the rows already on screen.
			this.liveFailure = this.#describe(cause);
		} finally {
			this.#ticking = false;
		}
	}

	/** How a `Walk` hears that a page landed, or failed to. */
	watch(land: (at: PageState | null) => void) {
		this.#land = land;
	}
}

export type WalkSpec<Row> = {
	/** The key *this* listing sorted by: `timestamp`, or `last_seen`. */
	key: (row: Row) => string;
	peekID: () => string | null;
	/**
	 * The panel's own row when it is off the page. Caller-supplied, because a
	 * filtered sessions listing sorts by a span the session's own `last_seen` is
	 * not, and placing a row by the wrong span is worse than not placing it
	 * (spec 009 #14, divergence 1).
	 */
	showing: () => Ordered | null;
	open: (id: string) => void;
	/**
	 * A listing read oldest first — a dataset's items and a run's, which walk
	 * `seq` (spec 014 #21). The pure part orders newest first by `key`, so the
	 * walk turns its steps round rather than asking each caller to invert its
	 * key (spec 016 #3).
	 */
	ascending?: boolean;
};

/**
 * The panel's place in the listing's order and the gestures that move it — the
 * order, not the page's indices, because the panel's row can be off the page
 * entirely (spec 009 #13). The comparison is `peek.ts`, tests included; this is
 * the reactive wiring around it.
 */
export class Walk<Row extends { id: string }> {
	#listing!: Listing<Row>;
	#spec!: WalkSpec<Row>;
	/** A walk that ran out of page and turned it (spec 009 #6). */
	#rolling = $state.raw<Rolling | null>(null);
	/** Which way "down the page" is in the pure part's newest-first terms. */
	#down: 1 | -1 = 1;
	// The page as the pure part reads it: newest first by key. An ascending
	// listing is handed over reversed, so `neighbour` and `settled` see the
	// order they were written for and the steps below are turned round to match.
	#ordered = $derived.by(() => {
		const ordered = this.#listing.rows.map((row) => ({ id: row.id, key: this.#spec.key(row) }));
		return this.#down === 1 ? ordered : ordered.reverse();
	});

	position = $derived(anchor(this.#ordered, this.#spec.peekID(), this.#spec.showing()));
	// Dead while a page is in flight: the cursors on screen belong to the page
	// being left, so a walk would turn back the turn already asked for (PR #11,
	// sixth review; the bar goes dead for the same reason).
	hasPrev = $derived(
		!this.#listing.loading &&
			walkable(this.#ordered, this.position, this.#step(-1), this.#listing.prevCursor)
	);
	hasNext = $derived(
		!this.#listing.loading &&
			walkable(this.#ordered, this.position, this.#step(1), this.#listing.nextCursor)
	);

	/** A step down the page, in the order the rows are actually in. */
	#step(by: 1 | -1): 1 | -1 {
		return (by * this.#down) as 1 | -1;
	}

	constructor(listing: Listing<Row>, spec: WalkSpec<Row>) {
		this.#listing = listing;
		this.#spec = spec;
		this.#down = spec.ascending ? -1 : 1;
		listing.watch((at) => {
			const id = at && settled(this.#rolling, at, this.#ordered);
			this.#rolling = null;
			if (id) spec.open(id);
		});
	}

	step(by: 1 | -1) {
		// Nowhere to walk from until the panel's row says where it sits, and
		// nowhere while a page is in flight.
		if (this.position === null || this.#listing.loading) return;
		const id = neighbour(this.#ordered, this.position, this.#step(by));
		if (id) return this.#spec.open(id);
		// The cursor is the page's, whichever way its rows read: `next` is
		// always the page after this one.
		const cursor = by === 1 ? this.#listing.nextCursor : this.#listing.prevCursor;
		if (cursor === null) return;
		const direction = by === 1 ? 'next' : 'prev';
		this.#rolling = { from: this.position, step: this.#step(by), cursor, direction };
		this.#listing.turn({ cursor, direction });
	}
}
