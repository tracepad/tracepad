import { ABSENT, cost, count, duration } from '$lib/format';
import { billedTokens, tokenClasses, type Tokens } from '$lib/tokens';
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
	/**
	 * How many of one user's sessions began in the bucket. Present only on a
	 * `user_id` timeline (spec 023 #6): `stats_hourly` has no such number, so
	 * without the filter the key is absent rather than zero.
	 */
	sessions?: number;
	/**
	 * Token sums over the bucket's generations, per class (spec 031 #4, spec
	 * 049 #7). Each key is present only when something carried that count,
	 * and the object is absent when none did — absent, never zero, like
	 * `total_cost`.
	 */
	tokens?: Tokens;
	latency_ms: { p50?: number | null; p95?: number | null };
};

/** Aligned series, in the shape uPlot consumes: one x, one y per chart. */
export type Series = {
	/** Bucket starts as seconds since the epoch, ascending. */
	x: number[];
	count: (number | null)[];
	cost: (number | null)[];
	errors: (number | null)[];
	/** Null everywhere the answer carried no `sessions` at all (spec 023 #6). */
	sessions: (number | null)[];
	/** The five token classes, each null wherever the bucket carried none. */
	input: (number | null)[];
	output: (number | null)[];
	cacheRead: (number | null)[];
	reasoning: (number | null)[];
	cacheWrite: (number | null)[];
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

	const series: Series = {
		x: [],
		count: [],
		cost: [],
		errors: [],
		sessions: [],
		input: [],
		output: [],
		cacheRead: [],
		reasoning: [],
		cacheWrite: [],
		p50: [],
		p95: []
	};
	for (const at of [...instants].sort((a, b) => a - b)) {
		const bucket = byKey.get(key(at, window.bucket));
		series.x.push(at / 1000);
		series.count.push(bucket ? bucket.count : null);
		series.errors.push(bucket ? bucket.error_count : null);
		// A bucket with traffic but no priced trace has no cost at all.
		series.cost.push(bucket?.total_cost ?? null);
		// `sessions` is either on every bucket or on none; a bucket the
		// window drew and the server did not return is a gap here as it is
		// everywhere else.
		series.sessions.push(bucket?.sessions ?? null);
		// The same discipline as cost, per class: traffic whose calls
		// reported no usage is a gap on the Tokens chart, not a zero.
		series.input.push(bucket?.tokens?.input ?? null);
		series.output.push(bucket?.tokens?.output ?? null);
		series.cacheRead.push(bucket?.tokens?.cache_read ?? null);
		series.reasoning.push(bucket?.tokens?.reasoning ?? null);
		series.cacheWrite.push(bucket?.tokens?.cache_write ?? null);
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
	/**
	 * Input plus output tokens — what a bill is made of — and null when the
	 * group carried neither. Cache read is on the chart, not here: adding
	 * it would count the same tokens twice for the providers that report
	 * cached tokens inside the input (spec 031 #6).
	 */
	tokens: number | null;
	/** Every class the group reported, for the cell's tooltip (spec 049 #9). */
	tokenClasses: string | undefined;
	/** Bar widths as fractions of the largest row in each column. */
	countShare: number;
	errorShare: number;
	costShare: number;
	tokensShare: number;
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
		tokens: billedTokens(bucket.tokens),
		tokenClasses: tokenClasses(bucket.tokens),
		countShare: 0,
		errorShare: 0,
		costShare: 0,
		tokensShare: 0
	}));
	const peak = {
		count: Math.max(0, ...rows.map((row) => row.count)),
		errors: Math.max(0, ...rows.map((row) => row.errorCount)),
		cost: Math.max(0, ...rows.map((row) => row.cost ?? 0)),
		tokens: Math.max(0, ...rows.map((row) => row.tokens ?? 0))
	};
	for (const row of rows) {
		row.countShare = share(row.count, peak.count);
		row.errorShare = share(row.errorCount, peak.errors);
		// A group that reported no cost gets no bar rather than an empty
		// one: absent is not zero (spec 002 #14). Tokens likewise.
		row.costShare = row.cost === null ? 0 : share(row.cost, peak.cost);
		row.tokensShare = row.tokens === null ? 0 : share(row.tokens, peak.tokens);
	}
	return rows.sort((a, b) => b.count - a.count || a.key.localeCompare(b.key));
}

function share(value: number, peak: number): number {
	return peak > 0 ? value / peak : 0;
}

/** One tile of the dashboard's summary row (spec 034 #2), rendered. */
export type SummaryFigure = {
	id: 'traces' | 'cost' | 'errors' | 'latency';
	label: string;
	/** The figure, or a dash when the window carried none. */
	value: string;
	/** The previous window's figure, for the tooltip; null when there is none. */
	previous: string | null;
	/**
	 * The change against the previous window: a signed percentage, signed
	 * points, or a signed duration; `new` when the previous figure is absent,
	 * or is zero where the change would divide by it (traces and cost —
	 * Decision 13); null when this window's figure is absent.
	 */
	change: string | null;
	direction: 'up' | 'down' | 'flat' | null;
	/**
	 * Whether the movement is good or bad news. Cost, errors and latency
	 * colour an increase as worse; traces colour neither, because more
	 * traffic is the denominator, not a verdict.
	 */
	tone: 'better' | 'worse' | null;
};

/** The one word a change against nothing reads as. */
export const NEW = 'new';

/**
 * The four figures of the summary row with their movement against the
 * previous window (spec 034 #2). The comparison is arithmetic here and not
 * on the server: the API gives the two windows and never a `compare`.
 */
export function summarize(bucket: StatsBucket | null, previous: StatsBucket | null): SummaryFigure[] {
	const rate = (b: StatsBucket | null) =>
		b && b.count > 0 ? (b.error_count / b.count) * 100 : null;
	const p95 = (b: StatsBucket | null) => b?.latency_ms?.p95 ?? null;
	return [
		figure('traces', 'Traces', bucket?.count ?? null, previous?.count ?? null, count, percent, null),
		figure('cost', 'Cost', bucket?.total_cost ?? null, previous?.total_cost ?? null, cost, percent, 'worse'),
		figure('errors', 'Errors', rate(bucket), rate(previous), percentage, points, 'worse'),
		figure('latency', 'Latency', p95(bucket), p95(previous), duration, delta, 'worse')
	];
}

/** The changes that divide by the previous figure, and so have no answer against zero. */
const RATIOS = new Set<(value: number, previous: number) => string>();

function figure(
	id: SummaryFigure['id'],
	label: string,
	value: number | null,
	previous: number | null,
	render: (value: number) => string,
	change: (value: number, previous: number) => string,
	increase: 'worse' | null
): SummaryFigure {
	if (value === null) {
		return { id, label, value: ABSENT, previous: null, change: null, direction: null, tone: null };
	}
	const rendered = render(value);
	const before = previous === null ? null : render(previous);
	// An absent previous is a window before the project, and a zero is not a
	// base a percentage can be taken against: both read as new. A difference
	// — points, a duration — has an answer against zero, and gives it
	// (Decision 13): no errors last week and 3% this week is `+3 pt`, worse.
	if (previous === null || (previous === 0 && RATIOS.has(change))) {
		return { id, label, value: rendered, previous: before, change: NEW, direction: null, tone: null };
	}
	const direction = value > previous ? 'up' : value < previous ? 'down' : 'flat';
	const tone =
		increase === null || direction === 'flat' ? null : direction === 'up' ? 'worse' : 'better';
	return { id, label, value: rendered, previous: before, change: change(value, previous), direction, tone };
}

/** An error rate as the tile shows it. */
function percentage(value: number): string {
	return `${round(value)}%`;
}

function percent(value: number, previous: number): string {
	return signed(((value - previous) / previous) * 100, '%');
}
RATIOS.add(percent);

function points(value: number, previous: number): string {
	return signed(value - previous, ' pt');
}

/** A latency change, kept in the units `duration` would give the size. */
function delta(value: number, previous: number): string {
	if (value === previous) return '±0 ms';
	const size = duration(Math.abs(value - previous));
	return value > previous ? `+${size}` : `−${size}`;
}

/** Signed, with `±0` for no movement rather than a plus on nothing. */
function signed(value: number, unit: string): string {
	if (value === 0) return `±0${unit}`;
	return `${value < 0 ? '−' : '+'}${round(Math.abs(value))}${unit}`;
}

/** Whole numbers past ten, one decimal under it — enough to tell 1.2% from 1.9%. */
function round(value: number): string {
	return (Math.abs(value) >= 10 ? Math.round(value) : Math.round(value * 10) / 10).toString();
}
