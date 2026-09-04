import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { RUN_FILTERS, RUN_STATUSES, readRunFilters, runSearch } from './runs';

/**
 * The parity the trace and session filters are held to, pointed at the
 * endpoint spec 016 #2 added (spec 016 #13): the document the server serves is
 * read here, and a filter it accepts that the screen does not offer — or the
 * other way round — fails. The status vocabulary is read from the same place,
 * so the select cannot offer a state the API refuses.
 */
function documented() {
	const document = JSON.parse(
		readFileSync(resolve(process.cwd(), '../internal/server/openapi.json'), 'utf8')
	);
	const parameters = document.paths['/api/v1/runs'].get.parameters as Array<
		{ name: string; schema?: { enum?: string[] } } | { $ref: string }
	>;
	const plumbing = new Set(['limit', 'cursor', 'direction', 'count']);
	const filters: string[] = [];
	let statuses: string[] = [];
	for (const parameter of parameters) {
		if ('$ref' in parameter) {
			const name = parameter.$ref.split('/').pop() as string;
			const shared = document.components.parameters[name].name as string;
			if (!plumbing.has(shared)) filters.push(shared);
			continue;
		}
		if (!plumbing.has(parameter.name)) filters.push(parameter.name);
		if (parameter.name === 'status') statuses = parameter.schema?.enum ?? [];
	}
	return { filters, statuses };
}

describe('filter parity with the read API', () => {
	it('offers exactly the filters the API accepts', () => {
		expect([...RUN_FILTERS].sort()).toEqual(documented().filters.sort());
	});

	it('offers exactly the statuses the API accepts', () => {
		expect([...RUN_STATUSES].sort()).toEqual(documented().statuses.sort());
	});
});

describe('filters in the URL', () => {
	it('round-trips through a query string', () => {
		const filters = readRunFilters(new URLSearchParams('dataset=support-golden&status=running'));

		expect(filters).toEqual({ dataset: 'support-golden', status: 'running' });
		expect(runSearch(filters)).toBe('?dataset=support-golden&status=running');
	});

	it('drops what the API would refuse', () => {
		expect(runSearch({ dataset: '   ' })).toBe('');
		expect(readRunFilters(new URLSearchParams('dataset=&status=done'))).toEqual({});
	});
});
