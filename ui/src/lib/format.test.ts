import { describe, expect, it } from 'vitest';
import {
	ABSENT,
	axisDuration,
	bytes,
	cost,
	count,
	counted,
	fineDuration,
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

describe('fineDuration and axisDuration', () => {
	it('keep what a number under a millisecond has, and say <1 ms for a whole one', () => {
		expect(fineDuration(0.2)).toBe('0.2 ms');
		expect(fineDuration(0.4)).toBe('0.4 ms');
		expect(fineDuration(0.001)).toBe('<1 ms');
		expect(fineDuration(0)).toBe('<1 ms');
		expect(fineDuration(0.999)).toBe('<1 ms');
		expect(fineDuration(1)).toBe(duration(1));
		expect(fineDuration(1234)).toBe(duration(1234));
		expect(fineDuration(null)).toBe(ABSENT);
		expect(fineDuration(-1)).toBe(ABSENT);
	});

	// An axis from 0 to 0.4 ms has ticks at 0, 0.2 and 0.4; `<1 ms` three
	// times would be an axis that says nothing.
	it('writes an axis tick of zero as 0 ms', () => {
		expect(axisDuration(0)).toBe('0 ms');
		expect(axisDuration(0.2)).toBe('0.2 ms');
		expect(axisDuration(0.4)).toBe('0.4 ms');
		expect(axisDuration(842)).toBe(duration(842));
		expect(axisDuration(null)).toBe(ABSENT);
	});
});

describe('a span that ends before it starts', () => {
	// Two clocks that disagree: the sign is shown, and is never `<1 ms`, `-0`
	// or the em dash that would hide it (review of PR #202).
	it('keeps its sign through elapsed and wait', () => {
		expect(elapsed('2026-08-28T12:00:01Z', '2026-08-28T12:00:00.500Z')).toBe(-500);
		expect(wait(elapsed('2026-08-28T12:00:01Z', '2026-08-28T12:00:00.500Z'))).toBe('-500 ms');
		const barely = elapsed('2026-08-28T12:00:00.000500000Z', '2026-08-28T12:00:00.000300000Z');
		expect(barely).toBe(-1);
		expect(wait(barely)).toBe('-1 ms');
		expect(Object.is(elapsed('2026-08-28T12:00:00Z', '2026-08-28T12:00:00Z'), -0)).toBe(false);
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
