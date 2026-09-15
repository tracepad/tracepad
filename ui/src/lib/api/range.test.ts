import { describe, expect, it } from 'vitest';
import {
	BUCKETS,
	DEFAULT_PRESET,
	HOURLY_LIMIT_MS,
	PRESETS,
	defaultBucket,
	matchPreset,
	presetRange,
	previousRange,
	readBucket,
	readRange,
	spanMs
} from './range';

/**
 * The shared time window (spec 007 #7). Every screen filters by it, so the
 * round trip through the URL and the boundary the automatic bucket flips at
 * are worth pinning down: both are arithmetic that is invisible when wrong.
 */

const NOW = new Date('2026-09-08T12:00:00Z');

describe('presets', () => {
	it('resolve against the clock and leave the end open', () => {
		expect(presetRange('24h', NOW)).toEqual({ from: '2026-09-07T12:00:00.000Z' });
		// No `to`: "the last 24 hours" keeps ending now, which is what makes
		// the refresh control refresh rather than re-read a frozen slice.
		expect(presetRange('24h', NOW).to).toBeUndefined();
	});

	it('are recognised again in the window they produced', () => {
		for (const preset of PRESETS) {
			expect(matchPreset(presetRange(preset.key, NOW), NOW)).toBe(preset.key);
		}
	});

	it('survive the seconds between resolving one and rendering it', () => {
		const range = presetRange('1h', new Date(NOW.getTime() - 30_000));

		expect(matchPreset(range, NOW)).toBe('1h');
	});

	it('are not claimed by a window that merely looks similar', () => {
		// A closed window is never a preset: those always run up to now.
		expect(matchPreset({ from: presetRange('1h', NOW).from, to: NOW.toISOString() }, NOW)).toBeNull();
		expect(matchPreset({ from: '2026-09-08T09:13:00Z' }, NOW)).toBeNull();
		expect(matchPreset({}, NOW)).toBeNull();
		expect(matchPreset({ from: 'not a timestamp' }, NOW)).toBeNull();
	});
});

describe('the window in the URL', () => {
	it('round-trips what the API takes', () => {
		const params = new URLSearchParams('from=2026-09-01T00:00:00Z&to=2026-09-02T00:00:00Z');

		expect(readRange(params)).toEqual({
			from: '2026-09-01T00:00:00Z',
			to: '2026-09-02T00:00:00Z'
		});
	});

	it('ignores anything that is not a timestamp', () => {
		expect(readRange(new URLSearchParams('from=yesterday&to='))).toEqual({});
	});
});

describe('the bucket', () => {
	it('is hourly up to two days and daily above', () => {
		const at = (ms: number) => ({
			from: new Date(NOW.getTime() - ms).toISOString(),
			to: NOW.toISOString()
		});

		// The boundary itself is still hours: 48 points read as a shape.
		expect(defaultBucket(at(HOURLY_LIMIT_MS), NOW)).toBe('hour');
		expect(defaultBucket(at(HOURLY_LIMIT_MS + 1), NOW)).toBe('day');
		expect(defaultBucket(at(HOURLY_LIMIT_MS - 1), NOW)).toBe('hour');
	});

	it('measures an open window against now', () => {
		expect(spanMs({ from: presetRange('24h', NOW).from }, NOW)).toBe(86_400_000);
		expect(defaultBucket(presetRange('24h', NOW), NOW)).toBe('hour');
		expect(defaultBucket(presetRange('7d', NOW), NOW)).toBe('day');
		// Unbounded: nobody wants a chart of every hour since the epoch.
		expect(defaultBucket({}, NOW)).toBe('day');
	});

	it('prefers an explicit choice and falls back on nonsense', () => {
		const range = presetRange('7d', NOW);

		expect(readBucket(new URLSearchParams('group_by=hour'), range, NOW)).toBe('hour');
		expect(readBucket(new URLSearchParams('group_by=model'), range, NOW)).toBe('day');
		expect(readBucket(new URLSearchParams(), range, NOW)).toBe('day');
	});

	it('offers only what the endpoint groups a timeline by', () => {
		// `model` and `environment` are categories, not a timeline: they are
		// the breakdown tables, not the switcher (spec 007 #6).
		expect([...BUCKETS]).toEqual(['hour', 'day']);
	});
});

it('opens Stats on a preset the endpoint can serve', () => {
	expect(PRESETS.some((preset) => preset.key === DEFAULT_PRESET)).toBe(true);
});

// The previous window (spec 034 #2): the same length, ending where this one
// begins, with an open end read as now.
describe('previousRange', () => {
	it('mirrors the window before its start', () => {
		expect(previousRange({ from: '2026-09-01T12:00:00.000Z' }, NOW)).toEqual({
			from: '2026-08-25T12:00:00.000Z',
			to: '2026-09-01T12:00:00.000Z'
		});
		expect(
			previousRange({ from: '2026-09-01T00:00:00.000Z', to: '2026-09-03T00:00:00.000Z' }, NOW)
		).toEqual({ from: '2026-08-30T00:00:00.000Z', to: '2026-09-01T00:00:00.000Z' });
	});

	it('is nothing for a window with no start or no length', () => {
		expect(previousRange({}, NOW)).toBeNull();
		expect(previousRange({ to: '2026-09-03T00:00:00.000Z' }, NOW)).toBeNull();
		expect(previousRange({ from: '2026-09-03T00:00:00.000Z', to: '2026-09-01T00:00:00.000Z' }, NOW)).toBeNull();
	});
});
