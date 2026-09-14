import { describe, expect, it } from 'vitest';
import { billedTokens, breakdown, buildSeries, key, type StatsBucket } from './stats';

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

	it('keeps the three token classes apart, and absent ones absent', () => {
		const series = buildSeries(
			[
				bucket('2026-09-01T02:00:00Z', { tokens: { input: 300, output: 30, cache_read: 12 } }),
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
