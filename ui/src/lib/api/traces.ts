// The trace listing's own logic: which filters exist and how they travel in
// the URL. Pure, because that is what makes it worth testing without a DOM.

/**
 * Every filter `GET /api/v1/traces` accepts, in the order the API documents
 * them. This list is the contract: a parity test reads `openapi.json` and
 * fails if the API grew a filter this list does not have, or the other way
 * round (in the spirit of spec 004 #9), and the filter bar's fields are typed
 * against it so a filter that exists here but has no control does not compile.
 */
export const TRACE_FILTERS = [
	'from',
	'to',
	'q',
	'environment',
	'user_id',
	'session_id',
	'name',
	'tag',
	'status',
	'min_cost',
	'min_tokens',
	'release',
	'version',
	'type',
	'prompt',
	'run_id',
	'item_id'
] as const;

export type FilterName = (typeof TRACE_FILTERS)[number];

/** The filter state of the screen: absent means "not filtering on this". */
export type TraceFilters = {
	from?: string;
	to?: string;
	/** Full-text search (spec 011): a filter like the others, in the URL like the others. */
	q?: string;
	environment?: string;
	user_id?: string;
	session_id?: string;
	name?: string;
	tag?: string[];
	status?: 'error' | 'ok';
	min_cost?: string;
	/** Input plus output tokens at least this many (spec 049 #6). */
	min_tokens?: string;
	/** The deployment the trace ran in (spec 012 #4). */
	release?: string;
	/** The version of the trace's own logic. */
	version?: string;
	/** Traces containing at least one observation of this kind. */
	type?: string;
	/** `name` or `name@version`, which is also what the panel's badge links to. */
	prompt?: string;
	/** The dataset run a trace belongs to (spec 014 #2). */
	run_id?: string;
	/** The dataset item it answered. */
	item_id?: string;
};

/**
 * Reads the filters out of a URL. Every screen's state lives there so that
 * what somebody is looking at is a link they can send (Application contract).
 */
export function readFilters(params: URLSearchParams): TraceFilters {
	const filters: TraceFilters = {};
	for (const name of TRACE_FILTERS) {
		if (name === 'tag') {
			const tags = params.getAll('tag').filter(Boolean);
			if (tags.length) filters.tag = tags;
			continue;
		}
		const value = params.get(name)?.trim();
		if (!value) continue;
		if (name === 'status') {
			if (value === 'error' || value === 'ok') filters.status = value;
			continue;
		}
		filters[name] = value;
	}
	return filters;
}

/** How many filters are narrowing the listing right now. */
export function filterCount(filters: TraceFilters): number {
	return TRACE_FILTERS.filter((name) => {
		const value = filters[name];
		return Array.isArray(value) ? value.length > 0 : Boolean(value);
	}).length;
}

/**
 * Writes the filter state, plus whatever else the screen keeps in the URL,
 * back into a query string. Empty values are omitted rather than emptied: a
 * link should carry what is being filtered on and nothing else.
 */
export function filterSearch(filters: TraceFilters, extra: Record<string, string> = {}): string {
	const params = new URLSearchParams();
	for (const name of TRACE_FILTERS) {
		const value = filters[name];
		if (Array.isArray(value)) for (const tag of value) params.append(name, tag);
		else if (value) params.set(name, value);
	}
	for (const [name, value] of Object.entries(extra)) {
		if (value) params.set(name, value);
	}
	const encoded = params.toString();
	return encoded ? `?${encoded}` : '';
}

// `mergeRows` lived here until spec 009 #9: live mode folded a re-fetched
// first page into rows that accumulated, and with a window anchored at
// "newest" the page just fetched *is* what should be on screen. Merging would
// only grow the page past the size somebody chose.
