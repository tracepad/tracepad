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
 * The row `step` places from `current` among the ones the listing has loaded,
 * or `null` at either end — the panel's previous/next controls can only mean
 * the rows that are on screen (spec 008 #7).
 */
export function neighbour(
	ids: readonly string[],
	current: string | null,
	step: 1 | -1
): string | null {
	if (current === null) return null;
	const index = ids.indexOf(current);
	if (index < 0) return null;
	return ids[index + step] ?? null;
}

/**
 * A click the browser should keep for itself: a new tab, a new window, a
 * download, or any button but the primary one. The row is a real link to the
 * full page (spec 008 #3) and those gestures are the reason it is.
 */
export function modified(event: MouseEvent): boolean {
	return event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
}
