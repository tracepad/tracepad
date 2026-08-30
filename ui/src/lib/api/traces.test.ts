import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { TRACE_FILTERS, filterCount, filterSearch, readFilters } from './traces';

/**
 * The filter bar is a mirror of `GET /api/v1/traces`, and this is what keeps
 * it one: the document the server serves is read here, and any filter it
 * accepts that the interface does not offer — or the other way round — fails.
 * The same parity idea as spec 004 #9, pointed at the UI.
 *
 * Reading `openapi.json` rather than the generated types on purpose: the
 * generated file is derived from it, so a drift between them is already
 * caught by the gate's own check, and this test wants the source.
 */
function documentedFilters(): string[] {
	// Vitest runs with `ui/` as its root, so the document is one level up.
	const document = JSON.parse(
		readFileSync(resolve(process.cwd(), '../internal/server/openapi.json'), 'utf8')
	);
	const parameters = document.paths['/api/v1/traces'].get.parameters as Array<
		{ name: string } | { $ref: string }
	>;
	// Pagination and field selection are how the screen talks to the API, not
	// things a person filters on; everything else is a filter.
	const plumbing = new Set(['fields', 'limit', 'cursor', 'direction', 'count']);
	return parameters
		.map((parameter) => {
			if ('name' in parameter) return parameter.name;
			const name = parameter.$ref.split('/').pop() as string;
			return document.components.parameters[name].name as string;
		})
		.filter((name) => !plumbing.has(name));
}

describe('filter parity with the read API', () => {
	it('offers exactly the filters the API accepts', () => {
		expect([...TRACE_FILTERS].sort()).toEqual(documentedFilters().sort());
	});
});

describe('filters in the URL', () => {
	it('round-trips through a query string', () => {
		const filters = readFilters(
			new URLSearchParams(
				'environment=prod&status=error&tag=a&tag=b&min_cost=0.01&user_id=u1&' +
					'session_id=s1&name=chat&from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&' +
					'q=%22refund+failed%22'
			)
		);

		expect(filters).toEqual({
			environment: 'prod',
			status: 'error',
			tag: ['a', 'b'],
			min_cost: '0.01',
			user_id: 'u1',
			session_id: 's1',
			name: 'chat',
			from: '2026-08-01T00:00:00Z',
			to: '2026-08-02T00:00:00Z',
			q: '"refund failed"'
		});
		expect(filterCount(filters)).toBe(10);
		expect(readFilters(new URLSearchParams(filterSearch(filters)))).toEqual(filters);
	});

	// The search is a filter like the others: in the URL, so a search is a
	// link somebody can send (spec 011, Application contract).
	it('carries a search on its own', () => {
		expect(filterSearch({ q: 'refund failed' })).toBe('?q=refund+failed');
		expect(readFilters(new URLSearchParams('q=refund+failed')).q).toBe('refund failed');
	});

	it('drops empties so a link carries only what is filtered on', () => {
		const filters = readFilters(new URLSearchParams('environment=&tag=&name=%20'));

		expect(filters).toEqual({});
		expect(filterCount(filters)).toBe(0);
		expect(filterSearch(filters)).toBe('');
	});

	it('ignores a status the API would refuse', () => {
		expect(readFilters(new URLSearchParams('status=maybe')).status).toBeUndefined();
	});

	it('carries the rest of the screen state alongside the filters', () => {
		expect(filterSearch({ status: 'error' }, { live: '1' })).toBe('?status=error&live=1');
	});
});

// The live-poll merge that used to be tested here went with `mergeRows`
// (spec 009 #9): a window anchored at "newest" is replaced by the page it
// re-fetches, so there is nothing left to fold.
