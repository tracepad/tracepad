// The queue item listing's own logic (spec 024 #11): which filters
// `GET /api/v1/queues/{name}/items` accepts and how they travel in the URL.
// Pure, like the trace and run halves beside it, and held to `openapi.json` by
// the same parity test (spec 016 #13).

/** Every filter the item listing accepts. */
export const QUEUE_ITEM_FILTERS = ['status', 'annotator'] as const;

export type QueueItemFilterName = (typeof QUEUE_ITEM_FILTERS)[number];

/** The three states an item can be in (spec 024 #2), as the API spells them. */
export const ITEM_STATUSES = ['pending', 'completed', 'skipped'] as const;
export type ItemStatus = (typeof ITEM_STATUSES)[number];

/** The filter state of the screen: absent means "not filtering on this". */
export type QueueItemFilters = {
	status?: ItemStatus;
	annotator?: string;
};

/** Reads the filters out of a URL, so every view of a queue is a link. */
export function readQueueItemFilters(params: URLSearchParams): QueueItemFilters {
	const filters: QueueItemFilters = {};
	const status = params.get('status')?.trim();
	if (status && (ITEM_STATUSES as readonly string[]).includes(status)) {
		filters.status = status as ItemStatus;
	}
	const annotator = params.get('annotator')?.trim();
	if (annotator) filters.annotator = annotator;
	return filters;
}

/** Writes them back, dropping the blanks the API would refuse. */
export function queueItemSearch(filters: QueueItemFilters): string {
	const params = new URLSearchParams();
	for (const name of QUEUE_ITEM_FILTERS) {
		const value = filters[name]?.trim();
		if (value) params.set(name, value);
	}
	const encoded = params.toString();
	return encoded ? `?${encoded}` : '';
}
