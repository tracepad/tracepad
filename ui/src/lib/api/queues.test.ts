import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
	ITEM_STATUSES,
	QUEUE_ITEM_FILTERS,
	queueItemSearch,
	readQueueItemFilters
} from './queues';

/**
 * The parity the trace and run filters are held to, pointed at the item
 * listing spec 024 #8 added (spec 016 #13): the document the server serves is
 * read here, and a filter it accepts that the screen does not offer — or the
 * other way round — fails. The status vocabulary is read from the same place,
 * so the select cannot offer a state the API refuses.
 */
function documented() {
	const document = JSON.parse(
		readFileSync(resolve(process.cwd(), '../internal/server/openapi.json'), 'utf8')
	);
	const parameters = document.paths['/api/v1/queues/{name}/items'].get.parameters as Array<
		{ name: string; in?: string; schema?: { enum?: string[] } } | { $ref: string }
	>;
	const plumbing = new Set(['limit', 'cursor', 'direction', 'count']);
	const filters: string[] = [];
	let statuses: string[] = [];
	for (const parameter of parameters) {
		if ('$ref' in parameter) {
			const name = parameter.$ref.split('/').pop() as string;
			const shared = document.components.parameters[name];
			// The queue's own name is a path segment, not a filter.
			if (shared.in === 'query' && !plumbing.has(shared.name)) filters.push(shared.name);
			continue;
		}
		if (parameter.in === 'query' && !plumbing.has(parameter.name)) filters.push(parameter.name);
		if (parameter.name === 'status') statuses = parameter.schema?.enum ?? [];
	}
	return { filters, statuses };
}

describe('filter parity with the read API', () => {
	it('offers exactly the filters the API accepts', () => {
		expect([...QUEUE_ITEM_FILTERS].sort()).toEqual(documented().filters.sort());
	});

	it('offers exactly the statuses the API accepts', () => {
		expect([...ITEM_STATUSES].sort()).toEqual(documented().statuses.sort());
	});
});

describe('filters in the URL', () => {
	it('round-trips through a query string', () => {
		const filters = readQueueItemFilters(new URLSearchParams('status=skipped&annotator=ada'));

		expect(filters).toEqual({ status: 'skipped', annotator: 'ada' });
		expect(queueItemSearch(filters)).toBe('?status=skipped&annotator=ada');
	});

	it('drops what the API would refuse', () => {
		expect(queueItemSearch({ annotator: '   ' })).toBe('');
		expect(readQueueItemFilters(new URLSearchParams('status=done&annotator='))).toEqual({});
	});
});
