import type { Bucket } from './range';

// Turning `GET /api/v1/stats` into what the Stats screen draws. Pure, because
// the interesting part is arithmetic and the interesting failures are silent:
// a fabricated zero looks exactly like a measured one on a chart.

/** One bucket as the endpoint renders it. */
export type StatsBucket = {
	key: string;
	count: number;
	error_count: number;
	/** Absent when nothing in the bucket reported a cost — not zero. */
	total_cost?: number;
	latency_ms: { p50?: number | null; p95?: number | null };
};

/** Aligned series, in the shape uPlot consumes: one x, one y per chart. */
export type Series = {
	/** Bucket starts as seconds since the epoch, ascending. */
	x: number[];
	count: (number | null)[];
	cost: (number | null)[];
	errors: (number | null)[];
	p50: (number | null)[];
	p95: (number | null)[];
};

const STEP_MS: Record<Bucket, number> = { hour: 3_600_000, day: 86_400_000 };

/**
 * A ceiling on how many points a chart is asked to hold. It exists for the
 * absurd window rather than for the realistic one — 30 days of hours is 720
 * points, and uPlot is built for far more — so hitting it means the window
 * starts before anything this server has, and the grid is anchored to its end.
 */
const MAX_POINTS = 100_000;

/**
 * Builds the aligned series over the window, one point per bucket the window
 * spans. The x axis is filled in — time passes whether or not anything was
 * traced — but a bucket the server did not return stays `null`, and so does a
 * bucket that reported no cost. That is the whole point: a gap is information,
 * and drawing it as a zero would invent traffic that never happened (the API
 * already refuses to fabricate those rows; so does the screen).
 */
export function buildSeries(
	buckets: StatsBucket[],
	window: { from?: string; to?: string; bucket: Bucket; now: Date }
): Series {
	const step = STEP_MS[window.bucket];
	const byKey = new Map(buckets.map((bucket) => [bucket.key, bucket]));

	const instants = new Set<number>();
	// Whatever came back is on the axis whether or not the window agrees:
	// the data is the one thing here that is not a derivation.
	for (const bucket of buckets) {
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

	const series: Series = { x: [], count: [], cost: [], errors: [], p50: [], p95: [] };
	for (const at of [...instants].sort((a, b) => a - b)) {
		const bucket = byKey.get(key(at, window.bucket));
		series.x.push(at / 1000);
		series.count.push(bucket ? bucket.count : null);
		series.errors.push(bucket ? bucket.error_count : null);
		// A bucket with traffic but no priced trace has no cost at all.
		series.cost.push(bucket?.total_cost ?? null);
		series.p50.push(bucket?.latency_ms?.p50 ?? null);
		series.p95.push(bucket?.latency_ms?.p95 ?? null);
	}
	return series;
}

/**
 * The bucket key the server would write for an instant. It is UTC because the
 * server's `strftime` is: reading the keys back in the reader's own zone would
 * shift every point by the offset.
 */
export function key(at: number, bucket: Bucket): string {
	const iso = new Date(at).toISOString();
	return bucket === 'hour' ? `${iso.slice(0, 13)}:00:00Z` : iso.slice(0, 10);
}

/** An RFC 3339 instant floored to its bucket, plus an optional step offset. */
function boundary(instant: string | undefined, step: number, offsetMs = 0): number | null {
	if (!instant) return null;
	const at = Date.parse(instant) + offsetMs;
	if (Number.isNaN(at)) return null;
	// The epoch is on an hour and a day boundary in UTC, so flooring is
	// modular arithmetic rather than calendar arithmetic.
	return at - ((at % step) + step) % step;
}

/** One row of a `group_by=model|environment` breakdown table. */
export type BreakdownRow = {
	key: string;
	count: number;
	errorCount: number;
	/** Null when nothing in this group reported a cost. */
	cost: number | null;
	/** Bar widths as fractions of the largest row in each column. */
	countShare: number;
	errorShare: number;
	costShare: number;
};

/**
 * Turns categorical buckets into table rows with proportion bars, biggest
 * first (spec 007 #6).
 *
 * The bars are drawn against the largest row rather than against the total,
 * because the question a breakdown answers is "which of these is the big one",
 * and a share of the total renders every row of a twenty-model deployment as
 * the same invisible sliver. The number beside the bar is the absolute value,
 * so nothing is only expressed as a proportion.
 */
export function breakdown(buckets: StatsBucket[], unnamed = ''): BreakdownRow[] {
	const rows = buckets.map((bucket) => ({
		// The empty key is a group, not a gap: grouped by release, it is
		// every trace whose client named none, and dropping the row would
		// make the breakdown's numbers stop adding up (spec 012 #4). It
		// needs a name a person can read, which the caller supplies.
		key: bucket.key === '' && unnamed ? unnamed : bucket.key,
		count: bucket.count,
		errorCount: bucket.error_count,
		cost: bucket.total_cost ?? null,
		countShare: 0,
		errorShare: 0,
		costShare: 0
	}));
	const peak = {
		count: Math.max(0, ...rows.map((row) => row.count)),
		errors: Math.max(0, ...rows.map((row) => row.errorCount)),
		cost: Math.max(0, ...rows.map((row) => row.cost ?? 0))
	};
	for (const row of rows) {
		row.countShare = share(row.count, peak.count);
		row.errorShare = share(row.errorCount, peak.errors);
		// A group that reported no cost gets no bar rather than an empty
		// one: absent is not zero (spec 002 #14).
		row.costShare = row.cost === null ? 0 : share(row.cost, peak.cost);
	}
	return rows.sort((a, b) => b.count - a.count || a.key.localeCompare(b.key));
}

function share(value: number, peak: number): number {
	return peak > 0 ? value / peak : 0;
}
