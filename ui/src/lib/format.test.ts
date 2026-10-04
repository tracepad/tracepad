import { describe, expect, it } from 'vitest';
import {
	ABSENT,
	bytes,
	cost,
	count,
	counted,
	duration,
	elapsed,
	relative,
	timestamp,
	wait
} from './format';

describe('duration', () => {
	it('changes unit with magnitude', () => {
		expect(duration(42.37)).toBe('42.4 ms');
		expect(duration(842)).toBe('842 ms');
		expect(duration(1234)).toBe('1.23 s');
		expect(duration(42_100)).toBe('42.1 s');
		expect(duration(125_400)).toBe('2m 05s');
	});

	// The server keeps whole milliseconds, so a span under one arrives as 0.
	// `0 µs` read as a measured zero beside the same trace's `1 ms` in its tree
	// (found upgrading a live install): the finest thing said is "under a
	// millisecond", and the same figure is said the same way at every edge.
	it('is the same at every boundary, and never finer than a millisecond', () => {
		expect(duration(0)).toBe('<1 ms');
		expect(duration(0.4)).toBe('<1 ms');
		expect(duration(0.999)).toBe('<1 ms');
		expect(duration(1)).toBe('1 ms');
		expect(duration(999)).toBe('999 ms');
		expect(duration(1000)).toBe('1 s');
		expect(duration(60_000)).toBe('1m 00s');
	});

	it('renders a missing value as absent rather than zero', () => {
		expect(duration(null)).toBe(ABSENT);
		expect(duration(undefined)).toBe(ABSENT);
		expect(duration(Number.NaN)).toBe(ABSENT);
	});
});

describe('wait', () => {
	it('reads like a duration', () => {
		expect(wait(388)).toBe('388 ms');
		expect(wait(1234)).toBe('1.23 s');
	});

	// The API stores a completion start that precedes its span as sent and
	// documents the sign; a screen that renders it as `—` reports a
	// disagreeing clock as a missing measurement, and the CLI, which prints
	// the number, would disagree with the screen about the same row.
	it('keeps the sign of a wait that ran backwards', () => {
		expect(wait(-388)).toBe('-388 ms');
		expect(wait(-1234)).toBe('-1.23 s');
	});

	it('renders a missing value as absent', () => {
		expect(wait(null)).toBe(ABSENT);
		expect(wait(undefined)).toBe(ABSENT);
		expect(wait(Number.NaN)).toBe(ABSENT);
	});
});

describe('cost', () => {
	it('keeps the digits that distinguish small amounts', () => {
		expect(cost(0.0000123)).toBe('$0.000012');
		expect(cost(0.0042)).toBe('$0.0042');
		expect(cost(0.5)).toBe('$0.5000');
		expect(cost(12.3456)).toBe('$12.35');
	});

	it('separates a real zero from an absent price', () => {
		expect(cost(0)).toBe('$0');
		expect(cost(null)).toBe(ABSENT);
	});
});

describe('bytes', () => {
	it('reads the way a load button should', () => {
		expect(bytes(512)).toBe('512 B');
		expect(bytes(2048)).toBe('2 KB');
		expect(bytes(45_600)).toBe('46 KB');
		expect(bytes(2_400_000)).toBe('2.4 MB');
		expect(bytes(null)).toBe(ABSENT);
	});
});

describe('count', () => {
	it('groups thousands', () => {
		expect(count(12345)).toBe('12,345');
		expect(count(0)).toBe('0');
		expect(count(undefined)).toBe(ABSENT);
	});
});

describe('counted', () => {
	it('names what it counts, one or many, and is absent when the count is', () => {
		expect(counted(1, 'trace')).toBe('1 trace');
		expect(counted(0, 'trace')).toBe('0 traces');
		expect(counted(12345, 'session')).toBe('12,345 sessions');
		expect(counted(null, 'trace')).toBe(ABSENT);
	});
});

describe('timestamp', () => {
	it('refuses to render an unparseable instant as an epoch', () => {
		expect(timestamp('not a date')).toBe(ABSENT);
		expect(timestamp(null)).toBe(ABSENT);
	});

	it('renders a real instant', () => {
		expect(timestamp('2026-08-28T12:34:56Z')).not.toBe(ABSENT);
	});
});

describe('relative', () => {
	const now = Date.parse('2026-09-07T12:00:00Z');
	const ago = (iso: string) => relative(iso, now);

	it('picks the coarsest unit that still says something', () => {
		expect(ago('2026-09-07T11:59:30Z')).toMatch(/30 seconds ago/);
		expect(ago('2026-09-07T11:00:00Z')).toMatch(/1 hour ago/);
		expect(ago('2026-09-05T12:00:00Z')).toMatch(/2 days ago/);
		expect(ago('2026-06-07T12:00:00Z')).toMatch(/3 months ago/);
		expect(ago('2024-09-07T12:00:00Z')).toMatch(/2 years ago/);
	});

	it('reads a clock ahead of the browser as the future, not as absent', () => {
		expect(ago('2026-09-07T12:02:00Z')).toMatch(/in 2 minutes/);
	});

	it('answers the em dash for an instant that is not one', () => {
		expect(relative(null)).toBe(ABSENT);
		expect(relative('whenever')).toBe(ABSENT);
	});
});

describe('elapsed', () => {
	it('is the span between two instants, or nothing', () => {
		expect(elapsed('2026-08-28T12:00:00Z', '2026-08-28T12:00:01.500Z')).toBe(1500);
		expect(elapsed('2026-08-28T12:00:00Z', null)).toBeNull();
	});

	// The API's timestamps carry nanoseconds. A span of 0.2 ms across a
	// millisecond boundary is 0 to the server (which truncates the nanosecond
	// difference) and was 1 here, from two Dates: a trace read `0 µs` in its
	// header and `1 ms` in its tree.
	it('truncates at full precision, as the server does', () => {
		expect(elapsed('2026-08-28T12:00:00.000900000Z', '2026-08-28T12:00:00.001100000Z')).toBe(0);
		expect(elapsed('2026-08-28T12:00:00.000400000Z', '2026-08-28T12:00:00.001400000Z')).toBe(1);
		expect(elapsed('2026-08-28T12:00:00.0004Z', '2026-08-28T12:00:00.0019Z')).toBe(1);
		expect(elapsed('2026-08-28T12:00:00.5Z', '2026-08-28T12:00:00.5Z')).toBe(0);
		expect(duration(elapsed('2026-08-28T12:00:00.000900000Z', '2026-08-28T12:00:00.001100000Z'))).toBe('<1 ms');
		expect(duration(elapsed('2026-08-28T12:00:00.000400000Z', '2026-08-28T12:00:00.001400000Z'))).toBe('1 ms');
	});
});
