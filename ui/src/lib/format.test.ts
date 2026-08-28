import { describe, expect, it } from 'vitest';
import { ABSENT, bytes, cost, count, duration, elapsed, timestamp } from './format';

describe('duration', () => {
	it('changes unit with magnitude', () => {
		expect(duration(0.4)).toBe('400 µs');
		expect(duration(42.37)).toBe('42.4 ms');
		expect(duration(842)).toBe('842 ms');
		expect(duration(1234)).toBe('1.23 s');
		expect(duration(42_100)).toBe('42.1 s');
		expect(duration(125_400)).toBe('2m 05s');
	});

	it('renders a missing value as absent rather than zero', () => {
		expect(duration(null)).toBe(ABSENT);
		expect(duration(undefined)).toBe(ABSENT);
		expect(duration(Number.NaN)).toBe(ABSENT);
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

describe('timestamp', () => {
	it('refuses to render an unparseable instant as an epoch', () => {
		expect(timestamp('not a date')).toBe(ABSENT);
		expect(timestamp(null)).toBe(ABSENT);
	});

	it('renders a real instant', () => {
		expect(timestamp('2026-08-28T12:34:56Z')).not.toBe(ABSENT);
	});
});

describe('elapsed', () => {
	it('is the span between two instants, or nothing', () => {
		expect(elapsed('2026-08-28T12:00:00Z', '2026-08-28T12:00:01.500Z')).toBe(1500);
		expect(elapsed('2026-08-28T12:00:00Z', null)).toBeNull();
	});
});
