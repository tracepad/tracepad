// The query state the peek panel owns on a listing URL (spec 008 #2), and the
// two small decisions every listing that has a panel has to make the same way:
// which click is ours to intercept, and what "the next row" means.
//
// Everything here is a pure function over a `URLSearchParams`, so the panel's
// whole contract is testable without a browser and without a route.

import type { PageState } from './page';

/** `peek` names the row the panel is showing; on `/sessions` it is a session. */
export const PEEK = 'peek';
/** A session panel drilled one level into a trace (spec 008 #9). */
export const TRACE = 'trace';
/** The observation selected inside a trace, shared with `/traces/{id}`. */
export const OBS = 'obs';

const OWNED = [PEEK, TRACE, OBS] as const;

/** What the panel is showing, as read from a listing's URL. */
export type PeekState = {
	peek: string | null;
	trace: string | null;
};

export function readPeek(search: URLSearchParams): PeekState {
	return { peek: search.get(PEEK), trace: search.get(TRACE) };
}

/**
 * The listing's search string with the panel set to `next`. Parameters the
 * panel does not own — filters, the time range, live mode — are carried
 * through untouched, which is what keeps the panel a layer over a listing
 * rather than a place of its own.
 *
 * A key left out of `next` is *removed*: opening another row must not carry
 * the previous row's observation selection into it, and closing the panel
 * must not leave a `trace` behind for the next open to inherit.
 */
export function peekSearch(
	search: URLSearchParams,
	next: Partial<Record<(typeof OWNED)[number], string | null>> = {}
): string {
	const params = new URLSearchParams(search);
	for (const key of OWNED) {
		const value = next[key];
		if (value) params.set(key, value);
		else params.delete(key);
	}
	const query = params.toString();
	return query ? `?${query}` : '';
}

/**
 * A row as the listing orders it: newest first by `key`, ties broken by `id`
 * — the `(timestamp, id)` pair the server pages by (spec 009 #2). The key must
 * be the one *this* listing sorted by: on a filtered sessions listing that is
 * not the same span as the session's own `last_seen`.
 */
export type Ordered = { id: string; key: string };

/**
 * A key as something comparable as text. The API formats instants as UTC
 * `RFC3339Nano`, which trims trailing zeros, so `…:00Z` sorts *after*
 * `…:00.5Z` as it stands; padding the fraction back to nine digits restores
 * the order, to the same nanosecond the server sorts by. `Date.parse` would
 * not — it truncates to milliseconds, and one millisecond is a whole parallel
 * fan-out. A row the server could not date arrives with no key at all, and
 * sorts last here as it does there.
 */
function sortable(key: string): string {
	if (key === '') return '';
	const instant = key.endsWith('Z') ? key.slice(0, -1) : key;
	const dot = instant.indexOf('.');
	if (dot < 0) return `${instant}.000000000`;
	return instant.slice(0, dot + 1) + instant.slice(dot + 1).padEnd(9, '0');
}

/** That order, as a comparison: newest first, ties broken by id descending. */
function compare(a: Ordered, b: Ordered): number {
	const first = sortable(a.key);
	const second = sortable(b.key);
	if (first !== second) return first < second ? 1 : -1;
	return a.id === b.id ? 0 : a.id > b.id ? -1 : 1;
}

/**
 * The row `step` places from `at` on a page ordered newest first: the nearest
 * one strictly older (`1`) or strictly newer (`-1`), or `null` when the page
 * holds no such row — which is where the caller turns the page (spec 009 #6).
 * `at` need not be *on* the page, and that is the whole point: it can go
 * missing off either end, and one question answers every side (spec 009 #13).
 */
export function neighbour(
	page: readonly Ordered[],
	at: Ordered | null,
	step: 1 | -1
): string | null {
	if (at === null) return null;
	if (step === 1) return page.find((row) => compare(row, at) > 0)?.id ?? null;
	// The page is sorted, so the last row still newer than `at` is the nearest.
	let newer: string | null = null;
	for (const row of page) {
		if (compare(row, at) >= 0) break;
		newer = row.id;
	}
	return newer;
}

/** Whether a walk does anything: a row that way on this page, or a page to
 * turn to find one. */
export function walkable(
	page: readonly Ordered[],
	at: Ordered | null,
	step: 1 | -1,
	cursor: string | null
): boolean {
	return at !== null && (neighbour(page, at, step) !== null || cursor !== null);
}

/**
 * A walk that ran out of page and turned it (spec 009 #6): the row it walked
 * from, and the page it is waiting for. Held rather than acted on, because
 * clearing it when the turn aborts the load it interrupted would lose it,
 * while never clearing it let an unrelated load inherit it.
 */
export type Rolling = {
	from: Ordered;
	step: 1 | -1;
	cursor: string;
	direction: PageState['direction'];
};

/**
 * The row a landed page should open, or `null` if this is not the page the
 * walk asked for. Direction as well as cursor: on a page of one row the two
 * cursors are the same key, so a `‹` walk and a `›` click would otherwise
 * match each other's intent (PR #11, third review).
 *
 * It lands on the nearest row in the direction asked, and failing that the
 * nearest row at all — the walk carries its own row across the turn rather
 * than a side of the new page to enter at, because a side is right only when
 * the walk began at the edge it stepped off.
 */
export function settled(
	intent: Rolling | null,
	at: PageState,
	page: readonly Ordered[]
): string | null {
	if (!intent || intent.cursor !== at.cursor || intent.direction !== at.direction) return null;
	const { from, step } = intent;
	return neighbour(page, from, step) ?? neighbour(page, from, -step as 1 | -1);
}

/**
 * Where the panel's own row sits in that order: read off the page when it is
 * one of the rows on it, and otherwise off the detail the panel is showing.
 * `null` when neither can say, and a walk from an unknown position would be a
 * guess, so it does not move.
 */
export function anchor(
	page: readonly Ordered[],
	id: string | null,
	showing: Ordered | null = null
): Ordered | null {
	if (id === null) return null;
	return page.find((row) => row.id === id) ?? (showing?.id === id ? showing : null);
}

/**
 * A click the browser should keep for itself: a new tab, a new window, a
 * download, or any button but the primary one. The row is a real link to the
 * full page (spec 008 #3) and those gestures are the reason it is.
 */
export function modified(event: MouseEvent): boolean {
	return event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
}

/**
 * Whether this click is somebody selecting text in the row rather than opening
 * it (spec 008 #15). A row is clickable end to end, and the values in it are
 * still values somebody copies out — a trace id into a `curl`, a user id into
 * a ticket — so a drag that ended in a selection is a selection, and the
 * second click of a double-click is a word.
 */
export function selecting(event: MouseEvent): boolean {
	if (event.detail > 1) return true;
	const selection = typeof window === 'undefined' ? null : window.getSelection();
	return selection !== null && !selection.isCollapsed;
}
