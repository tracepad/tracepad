import { describe, expect, it } from 'vitest';
import type { ComparedItem } from '$lib/api/client.svelte';
import {
	age,
	changedOnly,
	compareChoice,
	deltaText,
	itemBody,
	preview,
	savedMessage,
	scoreText
} from './evals';

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

// The item editor's submit (spec 016 #5). The three panes are text, and this
// is the parse that decides what — if anything — goes on the wire: the
// re-check spec 015 #15 asks a consumer to do rather than trusting a `valid`
// that settles 300 ms after the last keystroke.
describe('the item a draft becomes', () => {
	const draft = { input: '{"q": 1}', expected: '', metadata: '' };

	it('leaves an empty pane out rather than sending null', () => {
		expect(itemBody(draft)).toEqual({ item: { input: { q: 1 } } });
	});

	it('carries the id, the source pair and the two other bodies', () => {
		expect(
			itemBody({
				...draft,
				id: 'a'.repeat(32),
				expected: '{"a": "yes"}',
				metadata: '{"note": "cut from prod"}',
				sourceTraceID: 't1',
				sourceObservationID: 'o1'
			})
		).toEqual({
			item: {
				id: 'a'.repeat(32),
				input: { q: 1 },
				expected_output: { a: 'yes' },
				metadata: { note: 'cut from prod' },
				source_trace_id: 't1',
				source_observation_id: 'o1'
			}
		});
	});

	it('refuses an empty input, which is what a case is', () => {
		expect(itemBody({ ...draft, input: '  ' })).toEqual({
			problem: 'Input is what a case is; it cannot be empty.'
		});
	});

	it('names the pane that does not parse', () => {
		const answer = itemBody({ ...draft, expected: '{"a": }' });
		expect('problem' in answer && answer.problem).toMatch(/^Expected output is not JSON/);
	});
});

// The store's own answer, shown rather than hidden (spec 014 #6): a save that
// changed nothing wrote nothing, and only the server can say so.
describe('what a save is told', () => {
	it('distinguishes a write from a no-op', () => {
		expect(savedMessage({ version: 4, changed: 1 })).toBe('Saved as version 4.');
		expect(savedMessage({ version: 4, changed: 0 })).toMatch(/^Unchanged/);
		expect(savedMessage({ version: 4, changed: 0 })).toMatch(/version 4/);
	});
});
