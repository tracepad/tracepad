import { describe, expect, it } from 'vitest';
import type { AnnotationQueue, Score, ScoreConfig } from './api/client.svelte';
import { changed, deskBody, deskFields, progress, queueProblem, queueable, unfilled } from './queues';

// The rules of spec 024 that are not DOM nodes: the New-queue gate, how far a
// queue has got, the desk's prefilled form and its completeness rule, and what
// the traces listing may hand to a queue.

const config = (extra: Partial<ScoreConfig>): ScoreConfig => ({
	name: 'accuracy',
	data_type: 'numeric',
	direction: 'higher',
	min: 0,
	max: 1,
	categories: null,
	description: null,
	created_at: '2026-09-07T10:00:00Z',
	updated_at: '2026-09-07T10:00:00Z',
	...extra
});

const configs = [
	config({}),
	config({ name: 'tone', data_type: 'categorical', categories: ['warm', 'curt'], min: null, max: null })
];

const score = (extra: Partial<Score>): Score => ({
	id: 'a'.repeat(32),
	trace_id: 'b'.repeat(32),
	name: 'accuracy',
	data_type: 'numeric',
	value: 0.5,
	timestamp: '2026-09-07T10:00:00Z',
	created_at: '2026-09-07T10:00:00Z',
	...extra
});

describe('the New-queue gate', () => {
	it('mirrors the name grammar the server holds a queue to', () => {
		expect(queueProblem('weekly-review', ['accuracy'])).toBeNull();
		expect(queueProblem('', ['accuracy'])).toMatch(/needs a name/);
		expect(queueProblem('  ', ['accuracy'])).toMatch(/needs a name/);
		expect(queueProblem('-leading', ['accuracy'])).toMatch(/starts with a letter/);
		expect(queueProblem('has space', ['accuracy'])).toMatch(/starts with a letter/);
		expect(queueProblem('a'.repeat(201), ['accuracy'])).toMatch(/200 characters/);
	});

	it('refuses a queue that asks for nothing', () => {
		expect(queueProblem('weekly-review', [])).toMatch(/at least one score/);
	});
});

describe('how far a queue has got', () => {
	const queue = (counts: AnnotationQueue['counts']): AnnotationQueue => ({
		name: 'weekly',
		description: '',
		score_configs: ['accuracy'],
		counts,
		created_at: '2026-09-07T10:00:00Z',
		updated_at: '2026-09-07T10:00:00Z'
	});

	it('counts completed against everything the queue holds', () => {
		const at = progress(queue({ pending: 28, completed: 10, skipped: 2 }));

		expect(at.label).toBe('10 of 40');
		expect(at.completedPercent).toBe(25);
		// A skip is progress that produced no verdict, so it is its own share.
		expect(at.skippedPercent).toBe(5);
	});

	it('says nothing is done rather than dividing by nothing', () => {
		const at = progress(queue({ pending: 0, completed: 0, skipped: 0 }));

		expect(at.label).toBe('0 of 0');
		expect(at.completedPercent).toBe(0);
	});
});

describe("the desk's form", () => {
	it('asks for one control per score the queue names, in the queue order', () => {
		const fields = deskFields(['tone', 'accuracy'], configs, []);

		expect(fields.map((field) => field.name)).toEqual(['tone', 'accuracy']);
		expect(fields[0].config?.data_type).toBe('categorical');
	});

	// Decision 7's "whoever wrote it", made visible: a judge's verdict already
	// on the target is a verdict, and the reviewer confirms it.
	it('prefills from the scores already on the target', () => {
		const fields = deskFields(['accuracy'], configs, [score({ value: 0.75, comment: 'close' })]);

		expect(fields[0].existing).not.toBeNull();
		expect(fields[0].form.number).toBe('0.75');
		expect(fields[0].form.comment).toBe('close');
		// And a prefilled form is complete, so Complete is offered at once.
		expect(unfilled(fields, configs)).toEqual([]);
		// Nothing changed, so nothing is posted.
		expect(changed(fields[0], configs)).toBe(false);
	});

	it('mirrors the completeness rule the server checks', () => {
		const fields = deskFields(['accuracy', 'tone'], configs, []);

		expect(unfilled(fields, configs)).toEqual(['accuracy', 'tone']);
		fields[0].form.number = '0.9';
		expect(unfilled(fields, configs)).toEqual(['tone']);
		fields[1].form.text = 'warm';
		expect(unfilled(fields, configs)).toEqual([]);
	});

	// The edge case: the config was deleted and the queue kept the name, so
	// the score is still writable — free-typed, with nothing bounding it.
	it('keeps a name whose config is gone, as a free-typed score', () => {
		const fields = deskFields(['vibes'], configs, []);

		expect(fields[0].config).toBeUndefined();
		expect(fields[0].form.dataType).toBe('text');
		fields[0].form.text = 'good';
		expect(unfilled(fields, configs)).toEqual([]);
		expect(deskBody(fields[0], configs, { trace_id: 'c'.repeat(32) }, { queue: 'w', annotator: 'ada' }).name).toBe(
			'vibes'
		);
	});

	it('stamps where the verdict came from, on a new score and on a correction', () => {
		const stamp = { source: 'annotation', queue: 'weekly', annotator: 'ada' };
		const target = { trace_id: 'c'.repeat(32) };

		const fresh = deskFields(['accuracy'], configs, []);
		fresh[0].form.number = '0.9';
		const created = deskBody(fresh[0], configs, target, { queue: 'weekly', annotator: 'ada' });
		expect(created.metadata).toEqual(stamp);
		// A new score carries an id the field minted, not none: the desk posts
		// the scores and then completes, and a completion that fails is
		// retried from the same form. Without the id the second post wrote a
		// second `accuracy` row on the trace (found in review).
		expect(created.id).toMatch(/^[0-9a-f]{32}$/);
		fresh[0].form.number = '0.8';
		expect(deskBody(fresh[0], configs, target, { queue: 'weekly', annotator: 'ada' }).id).toBe(
			created.id
		);

		const editing = deskFields(['accuracy'], configs, [score({ metadata: { source: 'api' } })]);
		editing[0].form.number = '1';
		const corrected = deskBody(editing[0], configs, target, { queue: 'weekly', annotator: 'ada' });
		// A correction is the same row written again (spec 003 #3), and the
		// reviewer is now its author.
		expect(corrected.id).toBe('a'.repeat(32));
		expect(corrected.metadata).toEqual(stamp);
		expect(changed(editing[0], configs)).toBe(true);
	});

	it('posts an observation item against its own observation', () => {
		const fields = deskFields(['accuracy'], configs, []);
		fields[0].form.number = '0.9';
		const body = deskBody(
			fields[0],
			configs,
			{ trace_id: 'c'.repeat(32), observation_id: 'd'.repeat(16) },
			{ queue: 'weekly', annotator: 'ada' }
		);

		expect(body.observation_id).toBe('d'.repeat(16));
	});
});

describe('what the traces listing may queue', () => {
	it('names the count the listing already holds', () => {
		expect(queueable({ value: 37, capped: false })).toEqual({
			label: '37 traces',
			blocked: null
		});
	});

	it('refuses above the cap, with the reason', () => {
		const answer = queueable({ value: 1000, capped: true });

		expect(answer.label).toBe('1,000+ traces');
		expect(answer.blocked).toMatch(/Narrow the filters/);
	});

	it('says what it can when the count could not be taken', () => {
		expect(queueable(null).blocked).toBeNull();
	});
});
