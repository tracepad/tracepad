import { ApiError, api, type Score, type ScoreConfig, type ScoreFilters } from './api/client.svelte';

// The one read the score blocks stand on (spec 022 #1, #2): a target's scores,
// asked for once beside the thing being read, plus the configs that say what
// their names mean. Two screens open it — a trace and a session — so it lives
// here rather than twice inside components, the way `$lib/listing.svelte.ts`
// holds the listing all three tables share.

/** The endpoint's ceiling, and this block's page (#1). */
export const PAGE = 500;

export class Scores {
	rows = $state.raw<Score[]>([]);
	/** What the names mean; empty when the project declared none. */
	configs = $state.raw<ScoreConfig[]>([]);
	loading = $state(true);
	failure = $state<string | null>(null);
	/** The target carries more than one page, and no second one is fetched. */
	more = $state(false);

	#filter: () => ScoreFilters;
	#written = $state(0);
	/** The target the rows on hand came from, so a change can be told apart. */
	#read_from = '';

	constructor(filter: () => ScoreFilters) {
		this.#filter = filter;
	}

	/**
	 * Reads, and re-reads whenever the target changes or a write of ours
	 * lands. Call it as the whole body of an `$effect`: it returns the abort,
	 * so a target left behind cannot answer over the one being read.
	 */
	watch() {
		const filter = this.#filter();
		this.#written;
		const controller = new AbortController();
		this.#read(filter, controller.signal);
		return () => controller.abort();
	}

	/** After a score of ours is written or retracted, the block re-reads (#4). */
	refresh() {
		this.#written++;
	}

	async #read(filter: ScoreFilters, signal: AbortSignal) {
		const from = JSON.stringify(filter);
		if (from !== this.#read_from) {
			// Dropped before the request, not after it — the same reason
			// `TraceDetail` drops the trace before loading the next one. A chip
			// carries live Edit and Delete, so the previous target's rows left
			// under the new one's heading are an offer to correct or retract a
			// judgement about something else; and the trace header would draw
			// them against the new trace's observation ids, which marks every
			// one of them *unknown observation* (found in review of PR #41).
			this.rows = [];
			this.more = false;
			this.#read_from = from;
		}
		this.loading = true;
		this.failure = null;
		try {
			const page = await api.listScores(filter, { limit: PAGE }, signal);
			this.rows = page.scores;
			this.more = page.next_cursor !== null;
		} catch (cause) {
			if (signal.aborted) return;
			this.rows = [];
			this.failure = cause instanceof ApiError ? cause.message : 'Failed to read the scores.';
		} finally {
			if (!signal.aborted) this.loading = false;
		}
		try {
			this.configs = (await api.listScoreConfigs(signal)).configs;
		} catch {
			// A name without its config still renders: the config only adds
			// the declared range to a tooltip and the controls to the dialog,
			// so failing to read it is not the block's failure to report.
		}
	}
}
