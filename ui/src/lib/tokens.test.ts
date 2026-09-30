import { describe, expect, it } from 'vitest';
import { ABSENT } from './format';
import { billedTokens, compact, tokenClasses } from './tokens';

describe('billedTokens', () => {
	it('is input plus output, and never adds the other classes (spec 049 #1, #3)', () => {
		expect(billedTokens({ input: 10, output: 100, reasoning: 40, cache_read: 5, cache_write: 6 })).toBe(110);
		expect(billedTokens({ input: 7 })).toBe(7);
		expect(billedTokens({ output: 8 })).toBe(8);
	});
	it('is null when neither was reported, not zero', () => {
		expect(billedTokens(undefined)).toBeNull();
		expect(billedTokens({})).toBeNull();
		expect(billedTokens({ reasoning: 9, cache_read: 9 })).toBeNull();
	});
});

describe('compact', () => {
	it('keeps a column narrow', () => {
		expect(compact(950)).toBe('950');
		expect(compact(12_400)).toBe('12.4k');
		expect(compact(12_000)).toBe('12k');
		expect(compact(3_140_000)).toBe('3.1M');
		expect(compact(0)).toBe('0');
	});
	it('says nothing rather than zero', () => {
		expect(compact(null)).toBe(ABSENT);
		expect(compact(undefined)).toBe(ABSENT);
	});
});

describe('tokenClasses', () => {
	it('lists every class reported, exact, in order', () => {
		expect(tokenClasses({ cache_write: 64, input: 1200, output: 340, reasoning: 1_280 })).toBe(
			'Input 1,200\nOutput 340\nReasoning 1,280\nCache write 64'
		);
	});
	it('is absent when nothing was', () => {
		expect(tokenClasses(undefined)).toBeUndefined();
		expect(tokenClasses({})).toBeUndefined();
	});
});
