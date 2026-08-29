// The query state the peek panel owns on a listing URL (spec 008 #2), and the
// two small decisions every listing that has a panel has to make the same way:
// which click is ours to intercept, and what "the next row" means.
//
// Everything here is a pure function over a `URLSearchParams`, so the panel's
// whole contract is testable without a browser and without a route.

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
 * — the `(timestamp, id)` pair the server itself pages by (spec 009 #2).
 */
export type Ordered = { id: string; key: string };

/**
 * That order, as a comparison. Timestamps arrive as RFC3339 with trailing
 * zeros trimmed, so they cannot be compared as strings: `…:00Z` would sort
 * *after* `…:00.5Z`, because `Z` sorts after `.`. Parsed instead, with two
 * rows inside the same millisecond falling back to the id — which is the
 * tie-break the server uses anyway, and which also catches the row that
 * arrived without a date to place it by.
 */
function compare(a: Ordered, b: Ordered): number {
	const first = Date.parse(a.key);
	const second = Date.parse(b.key);
	if (first !== second && Number.isFinite(first) && Number.isFinite(second)) return second - first;
	return a.id === b.id ? 0 : a.id > b.id ? -1 : 1;
}

/**
 * The row `step` places from `at` on a page ordered newest first: the nearest
 * one strictly older (`1`) or strictly newer (`-1`), or `null` when the page
 * holds no such row — which is where the caller turns the page (spec 009 #6).
 *
 * `at` need not be *on* the page, and that is the whole reason the search is
 * by order rather than by index: a live tick pushes the panel's row off the
 * oldest end, a sent link or a resized page can leave it off the newest one,
 * and one question answers every side it can go missing from (spec 009 #13).
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

/**
 * Where to land once a page has turned under a walk: the nearest row in the
 * direction asked, and failing that the nearest row at all.
 *
 * The walk carries its own row across the turn rather than a side of the new
 * page to enter at: a side is right only when the row was at the edge it
 * stepped off. The second reading covers a page holding nothing at all on the
 * side the reader was heading for.
 */
export function landing(page: readonly Ordered[], at: Ordered | null, step: 1 | -1): string | null {
	return neighbour(page, at, step) ?? neighbour(page, at, -step as 1 | -1);
}

/**
 * Where the panel's own row sits in that order: read off the page when it is
 * one of the rows on it, and otherwise off the detail the panel is already
 * showing. `null` when neither can say — the moment between a deep link and
 * its detail arriving — and a walk from an unknown position would be a guess,
 * so it does not move.
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
