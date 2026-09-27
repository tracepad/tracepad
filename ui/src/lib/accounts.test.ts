import { describe, expect, it } from 'vitest';
import { MAX_PASSWORD_BYTES, passwordProblem } from './accounts';

// A password's cap is what `bcrypt` reads — bytes, not characters (spec 028
// #31) — so the form refuses what the server would, before the round trip.
describe('passwordProblem', () => {
	it('takes a password of exactly the cap', () => {
		const password = 'p'.repeat(MAX_PASSWORD_BYTES);
		expect(passwordProblem(password, password)).toBeNull();
	});

	it('refuses one byte past it', () => {
		const password = 'p'.repeat(MAX_PASSWORD_BYTES + 1);
		expect(passwordProblem(password, password)).toContain('at most 72 bytes');
	});

	it('counts a letter outside plain ASCII as the bytes it takes', () => {
		// 37 Cyrillic letters: 37 characters, 74 bytes.
		const password = '\u0436'.repeat(37);
		expect(passwordProblem(password, password)).toContain('at most 72 bytes');
	});
});
