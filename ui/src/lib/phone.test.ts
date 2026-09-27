import { describe, expect, it } from 'vitest';
import { ABSENT } from './format';
import { folded } from './phone';

// The line a phone row folds its other columns into (spec 006 #18). A column
// can say "nothing here" as a dash; a line of dashes between dots says less
// than a shorter line does.

describe('folded', () => {
	it('joins what is there with a dot', () => {
		expect(folded(['production', '2.08 s', '$0.0054'])).toBe('production · 2.08 s · $0.0054');
	});

	it('leaves out what is absent, however it is absent', () => {
		expect(folded(['staging', ABSENT, null, undefined, '', '$0'])).toBe('staging · $0');
	});

	it('is empty when nothing is there', () => {
		expect(folded([ABSENT, null])).toBe('');
	});
});
