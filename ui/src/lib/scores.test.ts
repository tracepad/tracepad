import { describe, expect, it } from 'vitest';
import type { Score, ScoreConfig } from './api/client.svelte';
import {
	API_SOURCE,
	OTHER,
	boundsLabel,
	configOf,
	emptyScoreForm,
	formOfScore,
	isCut,
	nameOf,
	observationIDs,
	refusedField,
	scoreBody,
	scoreProblem,
	scoreSource,
	scoreValue,
	splitScores,
	typeOf,
	type ScoreForm
} from './scores';

// The rules a reader of a trace acts on (spec 022 Testing): what a chip says
// for each of the four types, where a score claims to come from, how one
// response feeds two surfaces, and what the dialog sends.

const score = (extra: Partial<Score> = {}): Score => ({
	id: 'a'.repeat(32),
	name: 'helpfulness',
	data_type: 'numeric',
	value: 0.9,
	timestamp: '2026-09-07T10:00:00Z',
	created_at: '2026-09-07T10:00:00Z',
	...extra
});

describe('the value a chip shows', () => {
	it('cuts a number to three significant digits', () => {
		expect(scoreValue(score({ value: 0.857142857 }))).toBe('0.857');
		expect(scoreValue(score({ value: 1 }))).toBe('1');
		expect(scoreValue(score({ value: 1234 }))).toBe('1230');
		expect(scoreValue(score({ value: 0 }))).toBe('0');
	});

	it('reads a boolean as a word, zero included', () => {
		const boolean = { data_type: 'boolean' } as const;
		expect(scoreValue(score({ ...boolean, value: 1 }))).toBe('yes');
		expect(scoreValue(score({ ...boolean, value: 0 }))).toBe('no');
	});

	it('shows a category as itself', () => {
		expect(scoreValue(score({ data_type: 'categorical', value: undefined, string_value: 'partial' })))
			.toBe('partial');
	});

	it('cuts text at the preview and gives the whole of it on expand', () => {
		const long = 'x'.repeat(400);
		const text = score({ data_type: 'text', value: undefined, string_value: long });

		expect(scoreValue(text)).toBe(`${'x'.repeat(120)}…`);
		expect(scoreValue(text, true)).toBe(long);
		expect(isCut(text)).toBe(true);
		expect(isCut(score({ data_type: 'text', value: undefined, string_value: 'short' }))).toBe(false);
	});

	it('answers the em dash for a value the score does not carry', () => {
		expect(scoreValue(score({ value: undefined }))).toBe('—');
		expect(scoreValue(score({ data_type: 'text', value: undefined }))).toBe('—');
	});
});

describe('where a score says it came from', () => {
	it('reads a string source out of the metadata', () => {
		expect(scoreSource(score({ metadata: { source: 'web' } }))).toBe('web');
		expect(scoreSource(score({ metadata: { source: 'python-sdk' } }))).toBe('python-sdk');
	});

	it('calls anything else api', () => {
		// A score with no source came from somewhere holding a key, which is
		// what *api* says; a structured source is not a word to print.
		expect(scoreSource(score())).toBe(API_SOURCE);
		expect(scoreSource(score({ metadata: {} }))).toBe(API_SOURCE);
		expect(scoreSource(score({ metadata: { source: '' } }))).toBe(API_SOURCE);
		expect(scoreSource(score({ metadata: { source: { name: 'judge' } } }))).toBe(API_SOURCE);
	});
});

describe('the bounds in the value tooltip', () => {
	const config = (extra: Partial<ScoreConfig>): ScoreConfig => ({
		name: 'accuracy',
		data_type: 'numeric',
		direction: 'higher',
		min: null,
		max: null,
		categories: null,
		description: null,
		created_at: '2026-09-07T10:00:00Z',
		updated_at: '2026-09-07T10:00:00Z',
		...extra
	});

	it('says what the config declared, and nothing when it declared neither', () => {
		expect(boundsLabel(config({ min: 0, max: 1 }))).toBe('0 … 1');
		expect(boundsLabel(config({ min: 0 }))).toBe('at least 0');
		expect(boundsLabel(config({ max: 100 }))).toBe('at most 100');
		expect(boundsLabel(config({}))).toBeUndefined();
		expect(boundsLabel(undefined)).toBeUndefined();
	});
});

describe('splitting one response between the two surfaces', () => {
	const known = new Set(['obs-1', 'obs-2']);

	it('keeps the trace scores in the header and files the rest by observation', () => {
		const scores = [
			score({ id: '1' }),
			score({ id: '2', observation_id: 'obs-1' }),
			score({ id: '3', observation_id: 'obs-1' }),
			score({ id: '4', observation_id: 'obs-2' })
		];

		const split = splitScores(scores, known);

		expect(split.header.map((one) => one.score.id)).toEqual(['1']);
		expect(split.byObservation.get('obs-1')?.map((one) => one.id)).toEqual(['2', '3']);
		expect(split.byObservation.get('obs-2')?.map((one) => one.id)).toEqual(['4']);
		// The count the header shows: everything that went elsewhere.
		expect(split.onObservations).toBe(3);
	});

	// The mutation the split must not survive: dropping the observation scores
	// from both surfaces would lose them entirely.
	it('shows every score somewhere', () => {
		const scores = [score({ id: '1' }), score({ id: '2', observation_id: 'obs-1' })];
		const split = splitScores(scores, known);

		const drawn = [
			...split.header.map((one) => one.score.id),
			...[...split.byObservation.values()].flat().map((one) => one.id)
		];
		expect(drawn.sort()).toEqual(['1', '2']);
	});

	it('takes a score whose observation is not in this trace into the header, flagged', () => {
		const split = splitScores([score({ id: '9', observation_id: 'gone' })], known);

		expect(split.header).toEqual([{ score: expect.objectContaining({ id: '9' }), unknown: true }]);
		expect(split.byObservation.size).toBe(0);
		// Still counted among the ones on observations: it names one.
		expect(split.onObservations).toBe(1);
	});

	it('collects every observation id of a trace, however deep', () => {
		expect(
			observationIDs([{ id: 'a', children: [{ id: 'b', children: [{ id: 'c' }] }] }])
		).toEqual(new Set(['a', 'b', 'c']));
		expect(observationIDs(undefined).size).toBe(0);
	});
});

describe('the dialog form', () => {
	const configs: ScoreConfig[] = [
		{
			name: 'accuracy',
			data_type: 'numeric',
			direction: 'higher',
			min: 0,
			max: 1,
			categories: null,
			description: 'the judge',
			created_at: '2026-09-07T10:00:00Z',
			updated_at: '2026-09-07T10:00:00Z'
		},
		{
			name: 'verdict',
			data_type: 'categorical',
			direction: null,
			min: null,
			max: null,
			categories: ['correct', 'partial', 'wrong'],
			description: null,
			created_at: '2026-09-07T10:00:00Z',
			updated_at: '2026-09-07T10:00:00Z'
		}
	];

	const form = (extra: Partial<ScoreForm> = {}): ScoreForm => ({
		...emptyScoreForm(),
		...extra
	});

	it('opens on the first declared name, and on the free path when there is none', () => {
		expect(emptyScoreForm(configs).picked).toBe('accuracy');
		expect(emptyScoreForm([]).picked).toBe(OTHER);
	});

	it('takes the type from the config the name was picked from', () => {
		expect(typeOf(form({ picked: 'accuracy' }), configs)).toBe('numeric');
		expect(typeOf(form({ picked: 'verdict' }), configs)).toBe('categorical');
		expect(configOf(form({ picked: 'verdict' }), configs)?.categories).toEqual([
			'correct',
			'partial',
			'wrong'
		]);
	});

	it('asks the free-name path for a type, and takes it once stated', () => {
		const free = form({ picked: OTHER, name: 'vibes' });

		expect(typeOf(free, configs)).toBe('');
		expect(scoreProblem(free, configs)).toMatch(/kind of value/);
		expect(scoreProblem({ ...free, dataType: 'text', text: 'good' }, configs)).toBeNull();
		expect(nameOf({ ...free, name: '  vibes  ' })).toBe('vibes');
	});

	it('refuses a form the server would refuse, in its own terms', () => {
		expect(scoreProblem(form(), configs)).toMatch(/needs a name/);
		expect(scoreProblem(form({ picked: 'accuracy' }), configs)).toMatch(/needs a value/);
		expect(scoreProblem(form({ picked: 'accuracy', number: 'high' }), configs)).toMatch(
			/not a number/
		);
		expect(scoreProblem(form({ picked: 'verdict' }), configs)).toMatch(/categories/);
		expect(scoreProblem(form({ picked: 'accuracy', number: '0.5' }), configs)).toBeNull();
	});

	it('sends the type it knows rather than leaving it to be inferred', () => {
		// The mutation: a config path that omits `data_type` would have a
		// boolean stored as numeric and a category as text (spec 003 #5).
		const boolean = form({ picked: OTHER, name: 'grounded', dataType: 'boolean', number: '1' });
		const body = scoreBody({ trace_id: 't' }, boolean, configs);

		expect(body).toMatchObject({
			trace_id: 't',
			name: 'grounded',
			data_type: 'boolean',
			value: 1,
			metadata: { source: 'web' }
		});
		expect(body.timestamp).toBeUndefined();
		expect(body.id).toBeUndefined();
	});

	it('sends a categorical value as a string', () => {
		const body = scoreBody(
			{ session_id: 's1' },
			form({ picked: 'verdict', text: 'partial', comment: ' good enough ' }),
			configs
		);

		expect(body).toMatchObject({
			session_id: 's1',
			data_type: 'categorical',
			string_value: 'partial',
			comment: 'good enough'
		});
		expect(body.value).toBeUndefined();
	});

	it('edits by resending the id, the metadata and the time the score already had', () => {
		// The mutation: an edit that posts without the id writes a second row
		// beside the one it meant to correct (spec 003 #3, spec 022 #10).
		const editing = score({
			id: 'b'.repeat(32),
			name: 'accuracy',
			value: 0.4,
			metadata: { source: 'python-sdk', judge: 'claude' },
			timestamp: '2026-09-01T08:00:00Z'
		});
		const body = scoreBody(
			{ trace_id: 't' },
			formOfScore({ ...editing, value: 0.9 }, configs),
			configs,
			editing
		);

		expect(body.id).toBe('b'.repeat(32));
		expect(body.value).toBe(0.9);
		expect(body.metadata).toEqual({ source: 'python-sdk', judge: 'claude' });
		expect(body.timestamp).toBe('2026-09-01T08:00:00Z');
	});

	it('opens a score whose name has no config on the free path, filled', () => {
		const seeded = formOfScore(
			score({ name: 'vibes', data_type: 'text', value: undefined, string_value: 'fine' }),
			configs
		);

		expect(seeded.picked).toBe(OTHER);
		expect(seeded.name).toBe('vibes');
		expect(seeded.dataType).toBe('text');
		expect(seeded.text).toBe('fine');
	});
});

describe('where the server refusal is shown', () => {
	it('puts a refusal at the control it is about, and the rest at Save', () => {
		for (const [message, field] of [
			['"name" is required', 'name'],
			['"name" must be at most 200 characters', 'name'],
			['value 1.5 is above the config\'s max 1', 'value'],
			['"maybe" is not among the config\'s categories (pass, fail)', 'value'],
			['"accuracy" is numeric in its config, got categorical', 'value'],
			['a numeric score needs a "value"', 'value'],
			['a score needs a "trace_id" or a "session_id"', null],
			['write queue is full, retry shortly', null]
		] as const) {
			expect(refusedField(message), message).toBe(field);
		}
	});
});
