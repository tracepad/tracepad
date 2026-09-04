// The run listing's own logic (spec 016 #2): which filters `GET /api/v1/runs`
// accepts and how they travel in the URL. Pure, like the trace and session
// halves beside it, and held to `openapi.json` by the same parity test
// (spec 016 #13).

/** Every filter the project-wide run listing accepts. */
export const RUN_FILTERS = ['dataset', 'status'] as const;

export type RunFilterName = (typeof RUN_FILTERS)[number];

/** The three states a run can be in (spec 014 #8), as the API spells them. */
export const RUN_STATUSES = ['running', 'finished', 'failed'] as const;
export type RunStatus = (typeof RUN_STATUSES)[number];

/** The filter state of the screen: absent means "not filtering on this". */
export type RunFilters = {
	dataset?: string;
	status?: RunStatus;
};

/** Reads the filters out of a URL, so every view is a link. */
export function readRunFilters(params: URLSearchParams): RunFilters {
	const filters: RunFilters = {};
	const dataset = params.get('dataset')?.trim();
	if (dataset) filters.dataset = dataset;
	const status = params.get('status')?.trim();
	if (status && (RUN_STATUSES as readonly string[]).includes(status)) {
		filters.status = status as RunStatus;
	}
	return filters;
}

/** Writes them back, dropping the blanks the API would refuse. */
export function runSearch(filters: RunFilters): string {
	const params = new URLSearchParams();
	for (const name of RUN_FILTERS) {
		const value = filters[name]?.trim();
		if (value) params.set(name, value);
	}
	const encoded = params.toString();
	return encoded ? `?${encoded}` : '';
}
