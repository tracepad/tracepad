import { describe, expect, it } from 'vitest';
import type { ComparedItem } from '$lib/api/client.svelte';
import { age, changedOnly, compareChoice, deltaText, preview, scoreText } from './evals';

const run = (id: string, dataset: string) => ({ id, dataset });

// The checkbox rule (spec 016 #11): Compare is live only for exactly two runs
// of one dataset, and every other state names its reason.
describe('the compare rule', () => {
	it('needs exactly two runs', () => {
		expect(compareChoice([]).enabled).toBe(false);
		expect(compareChoice([run('a', 'x')]).reason).toMatch(/two runs/);
		expect(compareChoice([run('a', 'x'), run('b', 'x'), run('c', 'x')]).reason).toMatch(/exactly two/);
	});

	it('refuses two datasets with the reason the server would give', () => {
		const choice = compareChoice([run('a', 'x'), run('b', 'y')]);
		expect(choice.enabled).toBe(false);
		expect(choice.reason).toMatch(/different datasets/);
	});

	it('enables two runs of one dataset', () => {
		expect(compareChoice([run('a', 'x'), run('b', 'x')]).enabled).toBe(true);
	});
});

function item(id: string, scores: Record<string, ComparedItem['scores'][string]>): ComparedItem {
	return { id, seq: 1, in: 'both', scores };
}

// The toggle (spec 016 #10) filters the page, not the query: rows whose every
// verdict is `same` go, and nothing in the header is touched by it because the
// header is not this function's input.
describe('the changed-only toggle', () => {
	it('hides rows whose every verdict is same', () => {
		const rows = [
			item('same', { accuracy: { a: 1, b: 1, verdict: 'same' } }),
			item('moved', { accuracy: { a: 0, b: 1, verdict: 'improved' } }),
			item('mixed', {
				accuracy: { a: 1, b: 1, verdict: 'same' },
				verdict: { a: 'pass', b: 'fail', verdict: 'changed' }
			})
		];
		expect(changedOnly(rows).map((row) => row.id)).toEqual(['moved', 'mixed']);
	});

	it('keeps a row with nothing to compare, which is a change of its own', () => {
		expect(changedOnly([item('one-sided', {})]).length).toBe(1);
	});
});

describe('the words a cell uses', () => {
	it('previews compact JSON and cuts on a character, not a byte', () => {
		expect(preview({ q: 'one' })).toBe('{"q":"one"}');
		expect(preview('é'.repeat(200), 10)).toBe('é'.repeat(9) + '…');
		expect(preview(null)).toBe('');
	});

	it('says how long a run has been open', () => {
		const now = Date.parse('2026-09-04T12:00:00Z');
		expect(age('2026-09-04T11:59:40Z', now)).toBe('just now');
		expect(age('2026-09-04T11:30:00Z', now)).toBe('30 min');
		expect(age('2026-09-04T09:15:00Z', now)).toBe('2 h 45 min');
		expect(age('2026-09-01T09:15:00Z', now)).toBe('3 d');
		expect(age(null, now)).toBe('');
	});

	it('renders a score and a delta the way a person writes them', () => {
		expect(scoreText(0.5)).toBe('0.5');
		expect(scoreText('pass')).toBe('pass');
		expect(scoreText(null)).toBe('—');
		expect(deltaText(0.05)).toBe('+0.05');
		expect(deltaText(-0.25)).toBe('-0.25');
		expect(deltaText(null)).toBe('—');
	});
});
