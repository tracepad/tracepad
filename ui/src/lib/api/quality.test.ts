import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import type { ScoreConfig, ScoreSeries } from './client.svelte';
import {
	axisRange,
	breakdownRows,
	buildScoreSeries,
	categoriesOf,
	figure,
	orderSeries,
	DEFAULT_QUALITY_GROUPING,
	QUALITY_FILTERS,
	QUALITY_GROUPINGS,
	qualitySearch,
	summarize
} from './quality';

/**
 * The parity idea the other filter sets are held to (spec 016 #13), pointed at
 * the endpoint spec 025 adds: the document the server serves is read here, and
 * a parameter it accepts that the screen does not offer — or the other way
 * round — fails.
 */
function openapi() {
	return JSON.parse(
		readFileSync(resolve(process.cwd(), '../internal/server/openapi.json'), 'utf8')
	);
}

type DocumentedParameter = { name: string; schema?: { enum?: string[]; default?: string } };

function parameters(): DocumentedParameter[] {
	const document = openapi();
	const declared = document.paths['/api/v1/stats/scores'].get.parameters as Array<
		DocumentedParameter | { $ref: string }
	>;
	return declared.map((parameter) => {
		if ('name' in parameter) return parameter;
		const name = parameter.$ref.split('/').pop() as string;
		return document.components.parameters[name] as DocumentedParameter;
	});
}

const window30 = { from: '2026-09-01T00:00:00Z', to: '2026-09-04T00:00:00Z' };
const bucket = { ...window30, bucket: 'day' as const, now: new Date('2026-09-04T00:00:00Z') };

const numeric: ScoreSeries = {
	name: 'hallucination',
	data_type: 'numeric',
	buckets: [
		{ key: '2026-09-01', count: 2, mean: 0.4, min: 0.2, max: 0.6 },
		// 2026-09-02 is deliberately missing: nothing was graded that day.
		{ key: '2026-09-03', count: 1, mean: 1, min: 1, max: 1 }
	]
};

describe('parity with the read API', () => {
	it('offers exactly the parameters the endpoint accepts', () => {
		expect([...QUALITY_FILTERS].sort()).toEqual(parameters().map((one) => one.name).sort());
	});

	it('offers exactly the groupings the endpoint accepts, with the same default', () => {
		const group = parameters().find((one) => one.name === 'group_by');
		expect([...QUALITY_GROUPINGS]).toEqual(group?.schema?.enum);
		expect(DEFAULT_QUALITY_GROUPING).toBe(group?.schema?.default);
	});
});

describe('shaping a series', () => {
	it('draws a point per bucket of the window and leaves a gap a gap', () => {
		const shape = buildScoreSeries(numeric, bucket);

		// Three days in the window, and the middle one was not graded.
		expect(shape.x).toHaveLength(3);
		expect(shape.primary[0].values).toEqual([0.4, null, 1]);
		expect(shape.counts).toEqual([2, null, 1]);
		expect(shape.total).toBe(3);
	});

	it('carries the extremes behind the mean, and only for a numeric name', () => {
		const shape = buildScoreSeries(numeric, bucket);

		expect(shape.primary.map((line) => line.label)).toEqual(['Mean']);
		expect(shape.extremes.map((line) => line.label)).toEqual(['Min', 'Max']);
		expect(shape.extremes[0].values).toEqual([0.2, null, 1]);
		expect(shape.extremes[1].values).toEqual([0.6, null, 1]);
	});

	it('reads a boolean name as its rate', () => {
		const shape = buildScoreSeries(
			{
				name: 'thumbs',
				data_type: 'boolean',
				buckets: [{ key: '2026-09-01', count: 4, rate: 0.75 }]
			},
			bucket
		);

		expect(shape.primary.map((line) => line.label)).toEqual(['Rate']);
		expect(shape.primary[0].values[0]).toBe(0.75);
		expect(shape.extremes).toEqual([]);
	});

	it('reads a categorical name as one line per value, and the shares sum to one', () => {
		const series: ScoreSeries = {
			name: 'verdict',
			data_type: 'categorical',
			buckets: [
				{ key: '2026-09-01', count: 4, categories: { pass: 3, fail: 1 } },
				{ key: '2026-09-03', count: 2, categories: { pass: 2 } }
			]
		};
		const shape = buildScoreSeries(series, bucket);

		expect(categoriesOf(series)).toEqual(['fail', 'pass']);
		expect(shape.primary.map((line) => line.label)).toEqual(['fail', 'pass']);
		// Every line is a share of its bucket, so a day's lines add up to 1 —
		// which is what makes two days of different sizes comparable.
		const first = shape.primary.map((line) => line.values[0] as number);
		expect(first.reduce((sum, value) => sum + value, 0)).toBeCloseTo(1);
		// A value absent from a bucket is a zero share, not a gap: it was
		// graded, and it was not this.
		expect(shape.primary[0].values[2]).toBe(0);
		expect(shape.primary[1].values[2]).toBe(1);
		// The middle day was not graded at all, and that is a gap.
		expect(shape.primary[0].values[1]).toBeNull();
	});

	it('gives every line of a series a token of its own', () => {
		const shape = buildScoreSeries(
			{
				name: 'verdict',
				data_type: 'categorical',
				buckets: [{ key: '2026-09-01', count: 3, categories: { a: 1, b: 1, c: 1 } }]
			},
			bucket
		);

		expect(new Set(shape.primary.map((line) => line.token)).size).toBe(3);
	});
});

describe('the axis a config pins', () => {
	const config = (over: Partial<ScoreConfig>): ScoreConfig =>
		({ name: 'hallucination', data_type: 'numeric', ...over }) as ScoreConfig;

	it('takes the config range when both ends are set', () => {
		expect(axisRange(config({ min: 0, max: 1 }))).toEqual([0, 1]);
	});

	it('leaves the axis to the data when the config does not pin both ends', () => {
		expect(axisRange(undefined)).toBeUndefined();
		expect(axisRange(config({ min: 0 }))).toBeUndefined();
		expect(axisRange(config({ min: 1, max: 1 }))).toBeUndefined();
		expect(axisRange(config({ data_type: 'boolean', min: 0, max: 1 }))).toBeUndefined();
	});
});

describe('the card order', () => {
	const series = (name: string, data_type: ScoreSeries['data_type'] = 'numeric'): ScoreSeries => ({
		name,
		data_type,
		buckets: []
	});

	it('puts the configured names first, in the configs order', () => {
		const ordered = orderSeries(
			[series('zebra'), series('accuracy'), series('tone')],
			[{ name: 'tone' }, { name: 'accuracy' }] as ScoreConfig[]
		);

		expect(ordered.map((one) => one.name)).toEqual(['tone', 'accuracy', 'zebra']);
	});

	it('breaks a tie between two types of one name so the pair does not swap', () => {
		const ordered = orderSeries([series('mixed', 'numeric'), series('mixed', 'categorical')], []);

		expect(ordered.map((one) => one.data_type)).toEqual(['categorical', 'numeric']);
	});
});

describe('the breakdown rows', () => {
	it('sorts by count, names the empty key and bars against the biggest', () => {
		const rows = breakdownRows(
			{
				name: 'hallucination',
				data_type: 'numeric',
				buckets: [
					{ key: '', count: 1, mean: 0.5 },
					{ key: '2.6.0', count: 4, mean: 0.25 }
				]
			},
			'(no release)'
		);

		expect(rows.map((row) => row.key)).toEqual(['2.6.0', '(no release)']);
		expect(rows[0].countShare).toBe(1);
		expect(rows[1].countShare).toBe(0.25);
		expect(rows[0].summary).toBe('0.25');
	});

	it('reads each type the way its cell does', () => {
		expect(summarize('boolean', { key: 'a', count: 4, rate: 0.75 })).toBe('75%');
		expect(
			summarize('categorical', { key: 'a', count: 3, categories: { fail: 1, pass: 2 } })
		).toBe('pass 2 · fail 1');
		expect(summarize('numeric', { key: 'a', count: 1 })).toBe('—');
	});

	it('answers nothing for a grouping the range does not hold', () => {
		expect(breakdownRows(undefined)).toEqual([]);
	});
});

describe('a score value as somebody reads it', () => {
	it('keeps three significant digits and no trailing zeroes', () => {
		expect(figure(1 / 3)).toBe('0.333');
		expect(figure(0.25)).toBe('0.25');
		expect(figure(12.3456)).toBe('12.3');
		expect(figure(0)).toBe('0');
	});
});

describe('the URL state', () => {
	it('carries the window and the environment into a card', () => {
		const params = new URLSearchParams('from=2026-09-01T00%3A00%3A00Z&environment=production');

		expect(qualitySearch(params, { name: 'hallucination' })).toBe(
			'/quality?from=2026-09-01T00%3A00%3A00Z&environment=production&name=hallucination'
		);
	});

	it('drops a key an empty value clears, which is how the back link is written', () => {
		const params = new URLSearchParams('name=hallucination&group_by=hour');

		expect(qualitySearch(params, { name: '' })).toBe('/quality?group_by=hour');
	});

	it('is the bare route when nothing is set', () => {
		expect(qualitySearch(new URLSearchParams(), {})).toBe('/quality');
	});
});
