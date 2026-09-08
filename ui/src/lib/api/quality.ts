import type { ScoreBucket, ScoreConfig, ScoreSeries } from './client.svelte';
import { key } from './stats';
import type { Bucket } from './range';

// The Quality screen's own arithmetic (spec 025 #10, #11): turning
// `GET /api/v1/stats/scores` into what a card and a detail view draw.
//
// Pure, for the reason `stats.ts` is: the interesting part is division and the
// interesting failures are silent. A mean of means is a wrong number that looks
// like a right one, and a fabricated zero looks exactly like a measured one on
// a chart — so the endpoint's four numbers are read as they come, and a bucket
// the server did not return stays a gap.

/**
 * Every parameter `GET /api/v1/stats/scores` accepts. Like the three listings,
 * this is the contract: a parity test reads `openapi.json` and fails if the
 * endpoint grew one the screen does not offer, or the other way round
 * (spec 016 #13).
 */
export const QUALITY_FILTERS = ['from', 'to', 'environment', 'name', 'group_by'] as const;

/** The groupings the endpoint offers, in its own order. */
export const QUALITY_GROUPINGS = ['hour', 'day', 'environment', 'release', 'model'] as const;

/** What it groups by when the URL names nothing valid. */
export const DEFAULT_QUALITY_GROUPING = 'day';

/** The three breakdowns the detail view draws, in the order it draws them. */
export const QUALITY_BREAKDOWNS = [
	{ group: 'model', title: 'By model', label: 'Model' },
	{ group: 'environment', title: 'By environment', label: 'Environment' },
	{ group: 'release', title: 'By release', label: 'Release' }
] as const;

/**
 * The colour tokens a series' lines take, in order. A categorical name with
 * more values than this cycles it: six lines is already past what a reader can
 * tell apart, and inventing colours outside the palette would be inventing
 * contrast nobody checked.
 */
export const SERIES_TOKENS = ['accent', 'ok', 'warn', 'danger', 'code-key', 'code-number'];

/** One drawable line: the label uPlot's legend shows, and one value per bucket. */
export type ScoreLine = { label: string; values: (number | null)[]; token: string };

/**
 * A series in the shape a `Chart` consumes.
 *
 * `primary` is what the series *is* — the mean, the rate, or one line per
 * category — and `extremes` is the pair a numeric detail view draws behind it.
 * The card takes `primary` alone, which is what makes one function serve both
 * views (Decision 10, Decision 11).
 */
export type ScoreShape = {
	/** Bucket starts as seconds since the epoch, ascending. */
	x: number[];
	primary: ScoreLine[];
	extremes: ScoreLine[];
	counts: (number | null)[];
	/** How many scores the window holds for this series, over every bucket. */
	total: number;
};

const STEP_MS: Record<Bucket, number> = { hour: 3_600_000, day: 86_400_000 };

/**
 * A ceiling on how many points one series is asked to hold, the same one
 * `stats.ts` puts on its own axis. It exists for the absurd window rather than
 * for the realistic one — 30 days of hours is 720 points — so hitting it means
 * the window starts before anything this server has, and the grid is anchored
 * to its end.
 *
 * Without it `?from=1900-01-01&group_by=hour` builds about 1.1 million instants
 * *per series*, each one a `Date` and an ISO string in `key()`, and the tab
 * stops responding instead of drawing (found in review of PR #44).
 */
const MAX_POINTS = 100_000;

/**
 * Builds the aligned lines of one series over the window, one point per bucket
 * the window spans.
 *
 * The x axis is filled in — time passes whether or not anything was graded —
 * but a bucket the server did not return stays `null`. That is the whole point:
 * a gap means nothing was scored then, and drawing it as a zero would invent a
 * verdict of zero that nobody gave.
 */
export function buildScoreSeries(
	series: ScoreSeries,
	window: { from?: string; to?: string; bucket: Bucket; now: Date }
): ScoreShape {
	const step = STEP_MS[window.bucket];
	const byKey = new Map(series.buckets.map((bucket) => [bucket.key, bucket]));

	const instants = new Set<number>();
	// Whatever came back is on the axis whether or not the window agrees: the
	// data is the one thing here that is not a derivation.
	for (const bucket of series.buckets) {
		const at = Date.parse(bucket.key);
		if (!Number.isNaN(at)) instants.add(at);
	}
	const from = boundary(window.from, step);
	// `to` is exclusive, so the last bucket a window can hold is the one
	// containing the instant just before it.
	const to = window.to ? boundary(window.to, step, -1) : boundary(window.now.toISOString(), step);
	if (from !== null && to !== null && to >= from) {
		const start = Math.max(from, to - (MAX_POINTS - 1) * step);
		for (let at = start; at <= to; at += step) instants.add(at);
	}

	const x: number[] = [];
	const ordered: (ScoreBucket | undefined)[] = [];
	const counts: (number | null)[] = [];
	let total = 0;
	for (const at of [...instants].sort((a, b) => a - b)) {
		const bucket = byKey.get(key(at, window.bucket));
		x.push(at / 1000);
		ordered.push(bucket);
		counts.push(bucket ? bucket.count : null);
		total += bucket?.count ?? 0;
	}

	const line = (
		label: string,
		read: (bucket: ScoreBucket) => number | null,
		token: string
	): ScoreLine => ({
		label,
		values: ordered.map((bucket) => (bucket ? read(bucket) : null)),
		token
	});

	if (series.data_type === 'boolean') {
		return {
			x,
			primary: [line('Rate', (bucket) => bucket.rate ?? null, 'accent')],
			extremes: [],
			counts,
			total
		};
	}
	if (series.data_type === 'categorical') {
		return {
			x,
			// A share of the bucket rather than a count, so two buckets of
			// different sizes are comparable — which is the question a
			// categorical trend is asked.
			primary: categoriesOf(series).map((category, index) =>
				line(
					category,
					(bucket) => share(bucket.categories?.[category] ?? 0, bucket.count),
					SERIES_TOKENS[index % SERIES_TOKENS.length]
				)
			),
			extremes: [],
			counts,
			total
		};
	}
	return {
		x,
		primary: [line('Mean', (bucket) => bucket.mean ?? null, 'accent')],
		// Fainter, and behind: the mean is the reading, and the extremes are
		// the spread it came out of.
		extremes: [
			line('Min', (bucket) => bucket.min ?? null, 'subtle'),
			line('Max', (bucket) => bucket.max ?? null, 'subtle')
		],
		counts,
		total
	};
}

/** Every category a series' buckets mention, in a stable order. */
export function categoriesOf(series: ScoreSeries): string[] {
	const seen = new Set<string>();
	for (const bucket of series.buckets) {
		for (const category of Object.keys(bucket.categories ?? {})) seen.add(category);
	}
	return [...seen].sort();
}

/**
 * The y axis a numeric series is drawn against: the config's when it pins both
 * ends, and otherwise the data's own — which is what uPlot does when it is
 * given nothing (Decision 10, edge cases).
 */
export function axisRange(config: ScoreConfig | undefined): [number, number] | undefined {
	if (!config || config.data_type !== 'numeric') return undefined;
	const { min, max } = config;
	if (typeof min !== 'number' || typeof max !== 'number' || min >= max) return undefined;
	return [min, max];
}

/**
 * The card order (Decision 10): the score configs' order first — which is what
 * the project declared its names to mean — and the names nobody configured
 * after, alphabetically. A name graded two ways is two cards, and the type
 * breaks their tie so the pair does not swap places between reads.
 */
export function orderSeries(series: ScoreSeries[], configs: ScoreConfig[]): ScoreSeries[] {
	const declared = new Map(configs.map((config, index) => [config.name, index]));
	return [...series].sort((a, b) => {
		const left = declared.get(a.name) ?? Infinity;
		const right = declared.get(b.name) ?? Infinity;
		if (left !== right) return left - right;
		return a.name.localeCompare(b.name) || a.data_type.localeCompare(b.data_type);
	});
}

/** One row of a breakdown table: the key, the count, and the type's summary. */
export type ScoreBreakdownRow = {
	key: string;
	count: number;
	/** The mean, the rate, or the distribution, already rendered. */
	summary: string;
	/** The bar's width as a fraction of the biggest row. */
	countShare: number;
};

/**
 * Turns a grouped series into table rows, biggest first — the same rule and the
 * same bar as the statistics' own breakdowns (spec 007 #6).
 */
export function breakdownRows(series: ScoreSeries | undefined, unnamed = '—'): ScoreBreakdownRow[] {
	if (!series) return [];
	const peak = Math.max(0, ...series.buckets.map((bucket) => bucket.count));
	return series.buckets
		.map((bucket) => ({
			// The empty key is a group, not a gap: grouped by release it is
			// every trace whose client named none, and dropping the row would
			// make the numbers stop adding up (spec 012 #4).
			key: bucket.key === '' ? unnamed : bucket.key,
			count: bucket.count,
			summary: summarize(series.data_type, bucket),
			countShare: share(bucket.count, peak) ?? 0
		}))
		.sort((a, b) => b.count - a.count || a.key.localeCompare(b.key));
}

/** How one bucket reads in a table cell, by the series' type. */
export function summarize(dataType: ScoreSeries['data_type'], bucket: ScoreBucket): string {
	if (dataType === 'boolean') {
		return typeof bucket.rate === 'number' ? `${Math.round(bucket.rate * 100)}%` : '—';
	}
	if (dataType === 'categorical') {
		const entries = Object.entries(bucket.categories ?? {}).sort(
			(a, b) => b[1] - a[1] || a[0].localeCompare(b[0])
		);
		return entries.length ? entries.map(([value, n]) => `${value} ${n}`).join(' · ') : '—';
	}
	return typeof bucket.mean === 'number' ? figure(bucket.mean) : '—';
}

/**
 * A score value as somebody reads it back: three significant digits, the same
 * rendering the chips use (spec 022 #3). `toPrecision` alone leaves `0.250`,
 * and a trailing zero on a mean reads as a precision that is not claimed.
 */
export function figure(value: number): string {
	return Number(value.toPrecision(3)).toString();
}

function share(value: number, peak: number): number | null {
	return peak > 0 ? value / peak : null;
}

/** An RFC 3339 instant floored to its bucket, plus an optional step offset. */
function boundary(instant: string | undefined, step: number, offsetMs = 0): number | null {
	if (!instant) return null;
	const at = Date.parse(instant) + offsetMs;
	if (Number.isNaN(at)) return null;
	// The epoch is on an hour and a day boundary in UTC, so flooring is
	// modular arithmetic rather than calendar arithmetic.
	return at - (((at % step) + step) % step);
}

/**
 * The screen's whole URL state in one place: the window, the environment, the
 * bucket, and the name the detail view is about. An empty value removes the
 * key, which is how "back to the overview" is written.
 */
export function qualitySearch(
	params: URLSearchParams,
	next: Partial<Record<(typeof QUALITY_FILTERS)[number], string>>
): string {
	const carried = new URLSearchParams();
	for (const filter of QUALITY_FILTERS) {
		const value = next[filter] ?? params.get(filter) ?? '';
		if (value.trim()) carried.set(filter, value.trim());
	}
	const encoded = carried.toString();
	return encoded ? `/quality?${encoded}` : '/quality';
}
