import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
	FACET_FIELDS,
	FILTER_BOX_FROM,
	facetChip,
	facetOptions,
	narrowFacet,
	readList,
	writeList
} from './facets';

// The panel's own logic for a many-valued filter (spec 027 #6, #7). The
// interesting failures here are silent: a value that arrived from a link and
// quietly vanished from the list is a filter nobody can undo.

/**
 * The same parity idea as the trace listing's own test (spec 016 #13), pointed
 * at `/facets`: the three keys the answer carries have to be the three fields
 * this module offers, or a column the endpoint added is a column no control
 * ever shows.
 */
describe('parity with the read API', () => {
	it('offers exactly the columns the endpoint answers with', () => {
		const document = JSON.parse(
			readFileSync(resolve(process.cwd(), '../internal/server/openapi.json'), 'utf8')
		);
		const answer =
			document.paths['/api/v1/facets'].get.responses['200'].content['application/json'].schema;
		const columns = Object.keys(answer.properties).filter(
			(key) => !['from', 'to', 'omitted'].includes(key)
		);
		expect(columns.sort()).toEqual([...FACET_FIELDS].sort());
		// And `omitted` carries one number per column, or a capped list reads
		// as the whole list.
		expect(Object.keys(answer.properties.omitted.properties).sort()).toEqual(
			[...FACET_FIELDS].sort()
		);
	});
});

describe('the comma form', () => {
	it('round-trips through the URL', () => {
		expect(readList('production,staging')).toEqual(['production', 'staging']);
		expect(writeList(['production', 'staging'])).toBe('production,staging');
	});

	it('trims and collapses, as the server does', () => {
		expect(readList('production, staging ,production')).toEqual(['production', 'staging']);
		expect(writeList([' a ', 'a', 'b'])).toBe('a,b');
	});

	it('is nothing rather than empty, because a bare parameter is a 400', () => {
		expect(readList(undefined)).toEqual([]);
		expect(readList('')).toEqual([]);
		expect(writeList([])).toBeUndefined();
		expect(writeList(['  '])).toBeUndefined();
	});
});

describe('the checkbox list', () => {
	const values = [
		{ value: 'production', count: 4656 },
		{ value: 'staging', count: 12 },
		{ value: 'prod', count: 1 }
	];

	it('keeps the order the answer came in, which is by count', () => {
		expect(facetOptions(values, []).map((one) => one.value)).toEqual([
			'production',
			'staging',
			'prod'
		]);
	});

	it('marks what the URL carries', () => {
		const options = facetOptions(values, ['staging']);
		expect(options.find((one) => one.value === 'staging')?.checked).toBe(true);
		expect(options.find((one) => one.value === 'production')?.checked).toBe(false);
	});

	// A URL is a document: a link to `?environment=canary` from before canary
	// was retired must still show that it filters, and must still be undone.
	it('pins a checked value the range no longer holds, at the top and without a count', () => {
		const options = facetOptions(values, ['canary', 'staging']);
		expect(options[0]).toEqual({ value: 'canary', count: null, checked: true });
		expect(options).toHaveLength(4);
	});

	it('shows only the checked values while there are none to offer', () => {
		expect(facetOptions([], ['canary'])).toEqual([{ value: 'canary', count: null, checked: true }]);
		expect(facetOptions([], [])).toEqual([]);
	});
});

describe('the filter box', () => {
	const options = facetOptions(
		Array.from({ length: FILTER_BOX_FROM + 2 }, (_, i) => ({ value: `env-${i}`, count: 1 })),
		['env-3']
	);

	it('narrows by substring, folding case', () => {
		// `env-3` rides along on every query: it is checked. See below.
		expect(narrowFacet(options, 'ENV-5').map((one) => one.value)).toEqual(['env-3', 'env-5']);
	});

	it('keeps a checked value visible whatever is typed, so it can be unchecked', () => {
		expect(narrowFacet(options, 'env-1').map((one) => one.value)).toEqual(['env-1', 'env-3']);
	});

	it('does nothing with an empty query', () => {
		expect(narrowFacet(options, '   ')).toEqual(options);
	});
});

describe('the chip', () => {
	it('names up to two values, because a glance holds two', () => {
		expect(facetChip('Environment', ['production'])).toEqual({
			text: 'Environment: production',
			title: 'Environment: production'
		});
		expect(facetChip('Environment', ['production', 'staging']).text).toBe(
			'Environment: production, staging'
		);
	});

	it('counts beyond that, and keeps the names in the tooltip', () => {
		expect(facetChip('Environment', ['a', 'b', 'c'])).toEqual({
			text: 'Environment: 3 values',
			title: 'Environment: a, b, c'
		});
	});
});
