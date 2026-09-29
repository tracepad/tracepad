import { pageSearch, readPage } from '$lib/page';

// The user screens' own logic: which filters the listing has, which sorts the
// endpoint offers, and how the listing's and the page's state travel in the
// URL. `sessions.ts` is the model — a separate module per endpoint, because a
// shared "filters" type would have to lie about one of them.
//
// The URL writing lives here rather than in the two `.svelte` files for the
// reason every pure part of this interface does: it is the half with the
// interesting failures, and a test can hold it without a DOM.

/**
 * Every parameter `GET /api/v1/users` accepts beside the page plumbing. Like
 * the other two listings, this is the contract: a parity test reads
 * `openapi.json` and fails if the endpoint grew one the screen does not offer,
 * or the other way round (spec 016 #13).
 */
export const USER_FILTERS = ['sort', 'prefix'] as const;

export type UserFilterName = (typeof USER_FILTERS)[number];

/**
 * The questions the listing answers, in the order the endpoint declares them;
 * the first is its default. Always descending — ascending order of any of them
 * is nobody's question (spec 023 #5). `tokens` is input plus output, a user
 * with none last (spec 049 #6).
 */
export const USER_SORTS = [
	{ key: 'last_seen', label: 'Last seen' },
	{ key: 'traces', label: 'Traces' },
	{ key: 'cost', label: 'Cost' },
	{ key: 'tokens', label: 'Tokens' },
	{ key: 'errors', label: 'Errors' }
] as const;

export type UserSort = (typeof USER_SORTS)[number]['key'];

export const DEFAULT_USER_SORT: UserSort = 'last_seen';

/** The filter state of the screen: absent means "not filtering on this". */
export type UserFilters = {
	sort?: string;
	prefix?: string;
};

/** Reads them out of a URL, so every view is a link. */
export function readUserFilters(params: URLSearchParams): UserFilters {
	const filters: UserFilters = {};
	for (const name of USER_FILTERS) {
		const value = params.get(name)?.trim();
		if (value) filters[name] = value;
	}
	// A sort the endpoint does not know is a 400, and the screen has a
	// perfectly good answer for it: the default. The URL is hand-editable,
	// and a listing is not the place to argue about a typo (spec 009 #1's
	// posture on `limit`).
	if (filters.sort && !USER_SORTS.some((sort) => sort.key === filters.sort)) delete filters.sort;
	return filters;
}

/** Writes them back, dropping the blanks the API would refuse. */
export function userSearch(filters: UserFilters): string {
	const params = new URLSearchParams();
	for (const name of USER_FILTERS) {
		const value = filters[name]?.trim();
		if (value) params.set(name, value);
	}
	const encoded = params.toString();
	return encoded ? `?${encoded}` : '';
}

/** Which sort is in force, defaulted the way the endpoint defaults it. */
export function sortInForce(filters: UserFilters): UserSort {
	return (filters.sort as UserSort | undefined) ?? DEFAULT_USER_SORT;
}

/**
 * The two tabs of the user page (spec 023 #9). Sessions first, because a user
 * is a set of sessions and a trace of one is a click further in.
 */
export const USER_TABS = ['sessions', 'traces'] as const;
export type UserTab = (typeof USER_TABS)[number];

/** Which tab the URL names; anything else is the default. */
export function readTab(params: URLSearchParams): UserTab {
	const asked = params.get('tab');
	return USER_TABS.includes(asked as UserTab) ? (asked as UserTab) : 'sessions';
}

/**
 * The user page's search string with part of its state changed. An empty value
 * removes the key, which is how "clear the window" is written.
 *
 * The cursor goes either way: a window or a tab change starts the tab's
 * listing at its first page, because a cursor is a position in one listing and
 * means nothing in another. The page *size* is a preference and travels, the
 * way `freshSearch` carries it for the other listings. A tab change also
 * closes the panel, whose row belongs to the tab being left.
 */
export function userPageSearch(
	params: URLSearchParams,
	next: Partial<{ from: string; to: string; tab: string }>
): string {
	const carried = new URLSearchParams(params);
	for (const [name, value] of Object.entries(next)) {
		if (value) carried.set(name, value);
		else carried.delete(name);
	}
	if (next.tab !== undefined) for (const key of ['peek', 'obs', 'trace']) carried.delete(key);
	return pageSearch(carried, { limit: readPage(params).limit });
}
