// The query state a paginated listing owns (spec 009 #1): how big a page is,
// where it sits, and which way it was reached. Pure functions over a
// `URLSearchParams`, like the peek panel's own state beside it — the two share
// a URL and neither may touch the other's keys.

export const LIMIT = 'limit';
export const CURSOR = 'cursor';
export const DIRECTION = 'direction';

const OWNED = [LIMIT, CURSOR, DIRECTION] as const;

/** The steps the interface offers; the API takes anything from 1 to 500 (#5). */
export const PAGE_SIZES = [25, 50, 100, 250] as const;
export const DEFAULT_PAGE_SIZE = 50;

export type PageState = {
	limit: number;
	cursor: string | null;
	/** `prev` reads towards newer rows; with no cursor it is the oldest page. */
	direction: 'next' | 'prev';
};

/** What `GET /api/v1/traces` accepts; the steps above are the common ones. */
const MAX_PAGE_SIZE = 500;

export function readPage(search: URLSearchParams): PageState {
	const raw = Number(search.get(LIMIT));
	// Any size the API takes, not only the ones the control offers: the URL
	// is a link somebody can hand-edit, and refusing a size the server would
	// have served is the interface arguing with its own API. Anything else —
	// a word, a zero, a thousand — is the default rather than an error,
	// because a listing is not the place to have that argument either.
	const limit =
		Number.isInteger(raw) && raw >= 1 && raw <= MAX_PAGE_SIZE ? raw : DEFAULT_PAGE_SIZE;
	return {
		limit,
		cursor: search.get(CURSOR),
		direction: search.get(DIRECTION) === 'prev' ? 'prev' : 'next'
	};
}

/**
 * The listing's search string with the page set to `next`. Keys left out are
 * removed, so `pageSearch(search, { limit })` is "the first page at this size"
 * — which is what a filter change, a range change and a resize all need.
 *
 * The default size and the default direction are written as absent rather than
 * spelled out: a URL should carry what somebody chose, not the state of the
 * world.
 */
export function pageSearch(search: URLSearchParams, next: Partial<PageState> = {}): string {
	const params = new URLSearchParams(search);
	for (const key of OWNED) params.delete(key);
	if (next.limit !== undefined && next.limit !== DEFAULT_PAGE_SIZE) {
		params.set(LIMIT, String(next.limit));
	}
	if (next.cursor) params.set(CURSOR, next.cursor);
	if (next.direction === 'prev') params.set(DIRECTION, 'prev');
	const query = params.toString();
	return query ? `?${query}` : '';
}

/** True on the page a listing opens with, which is the only one live mode means anything on (#7). */
export function isFirstPage(state: PageState): boolean {
	return state.cursor === null && state.direction === 'next';
}

/**
 * True on the far end — the page « » reaches. Paired with `isFirstPage`,
 * these two say whether an *anchor* would move anything, which is a question
 * about the URL and not about the cursors of the page that arrived: an empty
 * page has no cursors at all, and it is exactly the page somebody needs the
 * anchors from (PR #11, second review).
 */
export function isLastPage(state: PageState): boolean {
	return state.cursor === null && state.direction === 'prev';
}
