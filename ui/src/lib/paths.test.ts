import { describe, expect, it } from 'vitest';
import { PREFIX, under, within } from './paths';

const P1 = 'a'.repeat(32);

// The shape of a project's URL (spec 029 #1, #2): written by `under`, read by
// `within`, and one is the other's inverse.
describe('the project prefix', () => {
	it('is written by under and taken off by within', () => {
		expect(under('/traces', P1)).toBe(`${PREFIX}/${P1}/traces`);
		expect(within(under('/settings/server', P1))).toBe('/settings/server');
		expect(within(under('/traces?status=error', P1))).toBe('/traces?status=error');
	});

	it('reads the front page as `/`, and a path with no prefix as it stands', () => {
		expect(within(`/p/${P1}`)).toBe('/');
		expect(within('/p')).toBe('/p');
		expect(within('/login')).toBe('/login');
	});
});
