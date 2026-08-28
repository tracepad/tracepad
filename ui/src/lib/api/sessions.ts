// The session listing's own logic: which filters exist and how they travel in
// the URL. The trace listing's `traces.ts` is the model; the two are separate
// because the endpoints are — a session takes four filters, not nine, and a
// shared "filters" type would have to lie about one of them.

/**
 * Every filter `GET /api/v1/sessions` accepts. Like the trace list, this is
 * the contract: a parity test reads `openapi.json` and fails if the endpoint
 * grew a filter the screen does not offer, or the other way round.
 */
export const SESSION_FILTERS = ['from', 'to', 'environment', 'user_id'] as const;

export type SessionFilterName = (typeof SESSION_FILTERS)[number];

/** The filter state of the screen: absent means "not filtering on this". */
export type SessionFilters = {
	from?: string;
	to?: string;
	environment?: string;
	user_id?: string;
};

/** Reads the filters out of a URL, so every view is a link. */
export function readSessionFilters(params: URLSearchParams): SessionFilters {
	const filters: SessionFilters = {};
	for (const name of SESSION_FILTERS) {
		const value = params.get(name)?.trim();
		if (value) filters[name] = value;
	}
	return filters;
}

/** Writes them back, dropping the blanks the API would refuse. */
export function sessionSearch(filters: SessionFilters): string {
	const params = new URLSearchParams();
	for (const name of SESSION_FILTERS) {
		const value = filters[name]?.trim();
		if (value) params.set(name, value);
	}
	const encoded = params.toString();
	return encoded ? `?${encoded}` : '';
}

/** How many filters are narrowing the listing right now. */
export function sessionFilterCount(filters: SessionFilters): number {
	return SESSION_FILTERS.filter((name) => Boolean(filters[name])).length;
}
