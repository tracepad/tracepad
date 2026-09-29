import { describe, expect, it } from 'vitest';
import { breakdown, buildSeries, key, NEW, summarize, type StatsBucket } from './stats';
import { billedTokens } from '$lib/tokens';

/**
 * What the Stats screen draws (spec 007 #6). Two things are worth pinning
 * down, because both are invisible when wrong: a bucket the server did not
 * return must stay a gap rather than become a zero, and the bars in a
 * breakdown must be proportions of something stated.
 */

const NOW = new Date('2026-09-01T05:30:00Z');

function bucket(id: string, over: Partial<StatsBucket> = {}): StatsBucket {
	return { key: id, count: 1, error_count: 0, latency_ms: { p50: 10, p95: 20 }, ...over };
}

describe('the time series', () => {
	it('fills the axis and leaves the data alone', () => {
		const series = buildSeries(
			[bucket('2026-09-01T02:00:00Z', { count: 7 }), bucket('2026-09-01T04:00:00Z', { count: 3 })],
			{ from: '2026-09-01T02:00:00Z', to: '2026-09-01T05:00:00Z', bucket: 'hour', now: NOW }
		);

		// Three hours: 02, 03, 04. `to` is exclusive, so 05:00 is not a bucket.
		expect(series.x.map((at) => new Date(at * 1000).toISOString())).toEqual([
			'2026-09-01T02:00:00.000Z',
			'2026-09-01T03:00:00.000Z',
			'2026-09-01T04:00:00.000Z'
		]);
		// The middle hour had no traffic, and a gap is what that is. A zero
		// there would draw an hour of measured silence that was never measured.
		expect(series.count).toEqual([7, null, 3]);
	});

	it('keeps an absent cost absent', () => {
		const series = buildSeries(
			[
				bucket('2026-09-01T02:00:00Z', { total_cost: 0.5 }),
				// Traffic, but nothing in it reported a cost.
				bucket('2026-09-01T03:00:00Z')
			],
			{ from: '2026-09-01T02:00:00Z', to: '2026-09-01T04:00:00Z', bucket: 'hour', now: NOW }
		);

		expect(series.count).toEqual([1, 1]);
		expect(series.cost).toEqual([0.5, null]);
	});

	it('keeps the five token classes apart, and absent ones absent', () => {
		const series = buildSeries(
			[
				bucket('2026-09-01T02:00:00Z', { tokens: { input: 300, output: 30, cache_read: 12, reasoning: 8, cache_write: 4 } }),
				// Traffic whose calls reported no usage: a gap on every class,
				// not three zeroes (spec 031 #6).
				bucket('2026-09-01T03:00:00Z'),
				// Only the OpenAI pair: cache read stays a gap on its own.
				bucket('2026-09-01T04:00:00Z', { tokens: { input: 200, output: 20 } })
			],
			{ from: '2026-09-01T02:00:00Z', to: '2026-09-01T05:00:00Z', bucket: 'hour', now: NOW }
		);

		expect(series.x).toHaveLength(3);
		expect(series.input).toEqual([300, null, 200]);
		expect(series.output).toEqual([30, null, 20]);
		expect(series.cacheRead).toEqual([12, null, null]);
		expect(series.reasoning).toEqual([8, null, null]);
		expect(series.cacheWrite).toEqual([4, null, null]);
	});

	it('carries the latency percentiles as two series', () => {
		const series = buildSeries([bucket('2026-09-01', { latency_ms: { p50: 40, p95: 900 } })], {
			from: '2026-09-01T00:00:00Z',
			to: '2026-09-02T00:00:00Z',
			bucket: 'day',
			now: NOW
		});

		expect(series.p50).toEqual([40]);
		expect(series.p95).toEqual([900]);
	});

	it('runs an open window up to now', () => {
		const series = buildSeries([], {
			from: '2026-09-01T03:00:00Z',
			bucket: 'hour',
			now: NOW
		});

		// 03, 04, 05 — the hour `now` falls in is the last one.
		expect(series.x).toHaveLength(3);
		expect(series.count).toEqual([null, null, null]);
	});

	it('shows a bucket the window does not cover rather than dropping it', () => {
		// Retention, clock skew, a window edited by hand: whatever the reason,
		// the data is the one thing here that is not a derivation.
		const series = buildSeries([bucket('2026-09-01T00:00:00Z', { count: 4 })], {
			from: '2026-09-01T03:00:00Z',
			to: '2026-09-01T04:00:00Z',
			bucket: 'hour',
			now: NOW
		});

		expect(series.count).toEqual([4, null]);
	});

	it('reads bucket keys in UTC, the way the server writes them', () => {
		const at = Date.parse('2026-09-01T02:30:00Z');

		expect(key(at, 'hour')).toBe('2026-09-01T02:00:00Z');
		expect(key(at, 'day')).toBe('2026-09-01');
	});
});

describe('the breakdown', () => {
	const rows = breakdown([
		bucket('claude-sonnet-5', { count: 100, error_count: 2, total_cost: 4 }),
		bucket('claude-haiku-4-5', { count: 25, error_count: 4 }),
		bucket('gpt-mini', { count: 50, error_count: 0, total_cost: 1 })
	]);

	it('puts the biggest first', () => {
		expect(rows.map((row) => row.key)).toEqual(['claude-sonnet-5', 'gpt-mini', 'claude-haiku-4-5']);
	});

	it('draws each bar against the largest row of its own column', () => {
		expect(rows[0].countShare).toBe(1);
		expect(rows[1].countShare).toBe(0.5);
		expect(rows[2].countShare).toBe(0.25);
		// Errors have their own scale: the busiest model is not the one that
		// fails most, and a shared scale would hide that.
		expect(rows[2].errorShare).toBe(1);
		expect(rows[1].costShare).toBe(0.25);
	});

	it('gives a group that reported no cost no bar at all', () => {
		const haiku = rows.find((row) => row.key === 'claude-haiku-4-5')!;

		expect(haiku.cost).toBeNull();
		expect(haiku.costShare).toBe(0);
	});

	it('counts a row by its input plus output, and never its cache read', () => {
		const rows = breakdown([
			bucket('claude-sonnet-5', { count: 3, tokens: { input: 300, output: 30, cache_read: 999 } }),
			bucket('gpt-mini', { count: 2, tokens: { output: 20 } }),
			// A model whose calls reported nothing: `—` in the column, no bar.
			bucket('local-llama', { count: 1 })
		]);

		expect(rows.map((row) => row.tokens)).toEqual([330, 20, null]);
		expect(rows.map((row) => row.tokensShare)).toEqual([1, 20 / 330, 0]);
		// Cache read alone is not a billed token either.
		expect(billedTokens({ cache_read: 5 })).toBeNull();
	});

	it('survives a column that is all zero', () => {
		const quiet = breakdown([bucket('a', { count: 0, error_count: 0 })]);

		expect(quiet[0].countShare).toBe(0);
		expect(quiet[0].errorShare).toBe(0);
	});
});

// The summary row (spec 034 #2): four figures and their movement against the
// previous window. The arithmetic is the one thing this screen computes, and
// the failures are silent — a division by a zero previous, a change coloured
// the wrong way — so each rule is pinned here.
describe('summarize', () => {
	const now = bucket('', {
		count: 120,
		error_count: 6,
		total_cost: 2.4,
		latency_ms: { p50: 500, p95: 1800 }
	});
	const before = bucket('', {
		count: 100,
		error_count: 2,
		total_cost: 3,
		latency_ms: { p50: 400, p95: 1500 }
	});

	it('renders the four figures with their changes', () => {
		const [traces, cost, errors, latency] = summarize(now, before);

		expect(traces).toMatchObject({ value: '120', change: '+20%', direction: 'up', tone: null });
		expect(cost).toMatchObject({ value: '$2.40', change: '−20%', direction: 'down', tone: 'better' });
		expect(errors).toMatchObject({ value: '5%', change: '+3 pt', direction: 'up', tone: 'worse' });
		expect(latency).toMatchObject({ value: '1.8 s', change: '+300 ms', direction: 'up', tone: 'worse' });
		expect(traces.previous).toBe('100');
		expect(latency.previous).toBe('1.5 s');
	});

	it('keeps a small change to one decimal and a flat one uncoloured', () => {
		const [traces, , errors] = summarize(
			bucket('', { count: 103, error_count: 5 }),
			bucket('', { count: 100, error_count: 5 })
		);

		expect(traces.change).toBe('+3%');
		// 4.85% against 5%: the tenth is what tells them apart.
		expect(errors.change).toBe('−0.1 pt');
		expect(summarize(now, now)[1]).toMatchObject({ change: '±0%', direction: 'flat', tone: null });
		expect(summarize(now, now)[3].change).toBe('±0 ms');
	});

	it('reads new against a zero or absent previous', () => {
		const [traces, cost, errors, latency] = summarize(now, null);
		for (const figure of [traces, cost, errors, latency]) {
			expect(figure.change).toBe(NEW);
			expect(figure.direction).toBeNull();
			expect(figure.previous).toBeNull();
		}
		// A previous window with traces but no cost, no errors and nothing
		// timed: the cost is new against it (a percentage of nothing), the
		// count is not, the error rate is a difference and says so
		// (Decision 13), and the untimed p95 is absent, so new.
		const quiet = summarize(now, bucket('', { count: 50, error_count: 0, latency_ms: {} }));
		expect(quiet[0].change).toBe('+140%');
		expect(quiet[1].change).toBe(NEW);
		expect(quiet[2]).toMatchObject({ change: '+5 pt', previous: '0%', direction: 'up', tone: 'worse' });
		expect(quiet[3].change).toBe(NEW);
		// No errors in either window is no movement, not news.
		const calm = summarize(
			bucket('', { count: 10, error_count: 0 }),
			bucket('', { count: 10, error_count: 0 })
		);
		expect(calm[2]).toMatchObject({ change: '±0 pt', direction: 'flat', tone: null });
	});

	it('dashes an absent figure and gives it no change', () => {
		const [traces, cost, errors, latency] = summarize(
			bucket('', { count: 0, error_count: 0, latency_ms: {} }),
			before
		);
		expect(traces).toMatchObject({ value: '0', change: '−100%', direction: 'down' });
		expect(cost).toMatchObject({ value: '—', change: null, direction: null, tone: null });
		expect(errors).toMatchObject({ value: '—', change: null });
		expect(latency).toMatchObject({ value: '—', change: null });

		for (const figure of summarize(null, before)) expect(figure.value).toBe('—');
	});

	it('renders the p95 change as a duration of the right size', () => {
		const [, , , latency] = summarize(
			bucket('', { count: 1, error_count: 0, latency_ms: { p50: 1, p95: 900 } }),
			bucket('', { count: 1, error_count: 0, latency_ms: { p50: 1, p95: 3400 } })
		);
		expect(latency).toMatchObject({ change: '−2.5 s', direction: 'down', tone: 'better' });
	});
});
