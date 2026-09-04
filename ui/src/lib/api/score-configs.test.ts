import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
	DATA_TYPES,
	DIRECTIONS,
	configBody,
	configProblem,
	emptyForm,
	formOf,
	type ConfigForm
} from './score-configs';

/**
 * The score-config form is a mirror of `PUT /api/v1/score-configs/{name}`
 * (spec 016 #13): the vocabularies it offers are the document's, and the rules
 * between its fields are spec 014 #16 said client-side. The server stays the
 * oracle — what is tested here is that the form does not refuse what the
 * server accepts, or accept what it refuses.
 */
function documentedEnum(property: 'data_type' | 'direction'): string[] {
	const document = JSON.parse(
		readFileSync(resolve(process.cwd(), '../internal/server/openapi.json'), 'utf8')
	);
	return document.components.schemas.ScoreConfigInput.properties[property].enum as string[];
}

const form = (over: Partial<ConfigForm> = {}): ConfigForm => ({
	...emptyForm(),
	name: 'accuracy',
	...over
});

describe('the vocabularies', () => {
	it('offers exactly the data types the API takes', () => {
		expect([...DATA_TYPES].sort()).toEqual(documentedEnum('data_type').sort());
	});

	it('offers exactly the directions the API takes', () => {
		expect([...DIRECTIONS].sort()).toEqual(documentedEnum('direction').sort());
	});
});

describe('what the form refuses', () => {
	it('wants a name', () => {
		expect(configProblem(form({ name: '  ' }))).toMatch(/needs a name/);
	});

	it.each(['numeric', 'boolean'] as const)('requires a direction on a %s name', (data_type) => {
		expect(configProblem(form({ data_type, direction: '' }))).toMatch(/which direction/);
		expect(configProblem(form({ data_type, direction: 'higher' }))).toBeNull();
	});

	it.each(['categorical', 'text'] as const)('refuses a direction on a %s name', (data_type) => {
		const categories = data_type === 'categorical' ? 'correct\nwrong' : '';
		expect(configProblem(form({ data_type, direction: 'higher', categories }))).toMatch(
			/no direction/
		);
		expect(configProblem(form({ data_type, direction: '', categories }))).toBeNull();
	});

	it('takes bounds on a number and nowhere else', () => {
		expect(configProblem(form({ min: '0', max: '1' }))).toBeNull();
		expect(configProblem(form({ data_type: 'boolean', direction: 'higher', min: '0' }))).toMatch(
			/Only a numeric name/
		);
	});

	it('refuses a minimum above the maximum, and a bound that is not a number', () => {
		expect(configProblem(form({ min: '2', max: '1' }))).toMatch(/above the maximum/);
		expect(configProblem(form({ min: 'half' }))).toMatch(/not a number/);
	});

	it('wants distinct categories on a categorical name and none anywhere else', () => {
		const categorical = { data_type: 'categorical' as const, direction: '' as const };
		expect(configProblem(form({ ...categorical, categories: '' }))).toMatch(/needs its categories/);
		expect(configProblem(form({ ...categorical, categories: 'good\ngood' }))).toMatch(/repeat/);
		expect(configProblem(form({ ...categorical, categories: 'good\nbad' }))).toBeNull();
		expect(configProblem(form({ categories: 'good' }))).toMatch(/no categories/);
	});
});

describe('the body it sends', () => {
	it('leaves out what the type does not take, and keeps a bound of zero', () => {
		expect(configBody(form({ min: '0', max: '1', description: '  ' }))).toEqual({
			data_type: 'numeric',
			direction: 'higher',
			min: 0,
			max: 1
		});
	});

	it('round-trips a stored config through the form', () => {
		const stored = {
			name: 'verdict',
			data_type: 'categorical' as const,
			direction: null,
			min: null,
			max: null,
			categories: ['correct', 'partial'],
			description: 'How the grader judged it',
			created_at: '2026-09-04T10:00:00Z',
			updated_at: '2026-09-04T10:00:00Z'
		};

		expect(configProblem(formOf(stored))).toBeNull();
		expect(configBody(formOf(stored))).toEqual({
			data_type: 'categorical',
			categories: ['correct', 'partial'],
			description: 'How the grader judged it'
		});
	});
});
