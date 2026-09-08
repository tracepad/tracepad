import { ApiError, api, type FacetValue } from './api/client.svelte';
import { FACET_FIELDS, facetLists, facetOmitted, type FacetField } from './api/facets';

// The one read behind every facet list (spec 027 #6): `GET /api/v1/facets` for
// the window in view, fetched **when the panel opens** and re-fetched when the
// window moves while it is open.
//
// Loading on open rather than with the page keeps the listing's first paint
// what it is — the panel is opened by a minority of visits — and one request
// carries all three columns, because three would be three spinners racing.
//
// Two callers open it: the filter panel on Traces, and the environment control
// on Sessions (Decision 8), which is why it lives here rather than inside a
// component.

const EMPTY: Record<FacetField, FacetValue[]> = { environment: [], release: [], name: [] };
const NONE: Record<FacetField, number> = { environment: 0, release: 0, name: 0 };

export class FacetValues {
	values = $state.raw<Record<FacetField, FacetValue[]>>(EMPTY);
	/** How many values the server's cap left out of each column (#2). */
	omitted = $state.raw<Record<FacetField, number>>(NONE);
	loading = $state(false);
	failure = $state<string | null>(null);

	#window: () => { from?: string; to?: string };
	#open: () => boolean;
	/** The window the values on hand describe, so a move can be told apart. */
	#read_for: string | null = null;

	constructor(window: () => { from?: string; to?: string }, open: () => boolean) {
		this.#window = window;
		this.#open = open;
	}

	/**
	 * Reads while the panel is open, and again when the window changes under
	 * it. Call it as the whole body of an `$effect`: it returns the abort, so
	 * a window left behind cannot answer over the one being asked about.
	 */
	watch() {
		const window = this.#window();
		if (!this.#open()) return;
		const key = `${window.from ?? ''}|${window.to ?? ''}`;
		if (key === this.#read_for) return;
		const controller = new AbortController();
		this.#read(window, key, controller.signal);
		return () => controller.abort();
	}

	async #read(window: { from?: string; to?: string }, key: string, signal: AbortSignal) {
		this.loading = true;
		this.failure = null;
		try {
			const answer = await api.getFacets(window, signal);
			if (signal.aborted) return;
			this.values = facetLists(answer);
			this.omitted = facetOmitted(answer);
			// Recorded only on success, so a failed read is retried the next
			// time the panel opens rather than remembered as an answer.
			this.#read_for = key;
		} catch (cause) {
			if (signal.aborted) return;
			// The values go with the failure: a stale list from another window
			// beside a line saying the read failed would be two claims about
			// one thing. The checked values still render, because they come
			// from the URL rather than from here (Decision 6).
			this.values = EMPTY;
			this.omitted = NONE;
			this.failure =
				cause instanceof ApiError ? cause.message : 'Failed to read the filter values.';
		} finally {
			if (!signal.aborted) this.loading = false;
		}
	}
}

export { FACET_FIELDS, type FacetField };
