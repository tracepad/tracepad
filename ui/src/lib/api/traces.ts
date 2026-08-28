import type { TraceRow } from './client.svelte';

// The trace listing's own logic: which filters exist, how they travel in the
// URL, and how a live poll folds a fresh first page into the rows already on
// screen. All of it pure, because all of it is worth testing without a DOM.

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
	'environment',
	'user_id',
	'session_id',
	'name',
	'tag',
	'status',
	'min_cost'
] as const;

export type FilterName = (typeof TRACE_FILTERS)[number];

/** The filter state of the screen: absent means "not filtering on this". */
export type TraceFilters = {
	from?: string;
	to?: string;
	environment?: string;
	user_id?: string;
	session_id?: string;
	name?: string;
	tag?: string[];
	status?: 'error' | 'ok';
	min_cost?: string;
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

/**
 * Folds a re-fetched first page into the rows on screen (spec 006 #12).
 *
 * Live mode re-reads the first page rather than asking for "everything since
 * X", so the new page and the old list overlap. Merging by id keeps a trace
 * that appears in both from appearing twice, and re-sorting on the listing's
 * own key — `timestamp DESC, id DESC` — keeps a late arrival in the place the
 * server would have put it rather than at the top.
 *
 * A row present in both wins from the incoming page: its aggregates (cost,
 * latency, error count) grow while the trace is still being written.
 */
export function mergeRows(existing: TraceRow[], incoming: TraceRow[]): TraceRow[] {
	const byID = new Map<string, TraceRow>();
	for (const row of existing) byID.set(row.id, row);
	for (const row of incoming) byID.set(row.id, row);
	return [...byID.values()].sort(compareRows);
}

function compareRows(a: TraceRow, b: TraceRow): number {
	const left = sortKey(a.timestamp);
	const right = sortKey(b.timestamp);
	if (left !== right) return left < right ? 1 : -1;
	return a.id < b.id ? 1 : a.id > b.id ? -1 : 0;
}

// RFC 3339 with a variable-length fraction does not compare as text: the
// server prints `…:00Z` and `…:00.5Z`, and `.` sorts before `Z`, which would
// put the later instant first. Padding the fraction to nanoseconds makes the
// strings comparable again — and keeps the nanosecond resolution that parsing
// into a `Date` would throw away.
const INSTANT = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?Z$/;

function sortKey(timestamp: string | undefined): string {
	if (!timestamp) return '';
	const parts = INSTANT.exec(timestamp);
	if (!parts) return timestamp;
	return `${parts[1]}.${(parts[2] ?? '').padEnd(9, '0')}`;
}
