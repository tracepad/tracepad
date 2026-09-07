import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
	DEFAULT_USER_SORT,
	readTab,
	readUserFilters,
	sortInForce,
	USER_FILTERS,
	USER_SORTS,
	userPageSearch,
	userSearch
} from './users';

/**
 * The parity idea the trace and session filters are held to (spec 016 #13),
 * pointed at the endpoint spec 023 adds: the document the server serves is read
 * here, and a parameter it accepts that the screen does not offer — or the
 * other way round — fails.
 */
function openapi() {
	return JSON.parse(
		readFileSync(resolve(process.cwd(), '../internal/server/openapi.json'), 'utf8')
	);
}

function documentedFilters(): string[] {
	const document = openapi();
	const parameters = document.paths['/api/v1/users'].get.parameters as Array<
		{ name: string } | { $ref: string }
	>;
	const plumbing = new Set(['limit', 'cursor', 'direction', 'count']);
	return parameters
		.map((parameter) => {
			if ('name' in parameter) return parameter.name;
			const name = parameter.$ref.split('/').pop() as string;
			return document.components.parameters[name].name as string;
		})
		.filter((name) => !plumbing.has(name));
}

describe('parity with the read API', () => {
	it('offers exactly the filters the API accepts', () => {
		expect([...USER_FILTERS].sort()).toEqual(documentedFilters().sort());
	});

	it('offers exactly the sorts the API accepts, with the same default', () => {
		const sort = openapi().paths['/api/v1/users'].get.parameters.find(
			(parameter: { name?: string }) => parameter.name === 'sort'
		);
		expect(USER_SORTS.map((one) => one.key)).toEqual(sort.schema.enum);
		expect(DEFAULT_USER_SORT).toBe(sort.schema.default);
	});
});

describe('the listing filters in the URL', () => {
	it('round-trips through a query string', () => {
		const filters = readUserFilters(new URLSearchParams('sort=cost&prefix=acme%3A'));

		expect(filters).toEqual({ sort: 'cost', prefix: 'acme:' });
		expect(userSearch(filters)).toBe('?sort=cost&prefix=acme%3A');
		expect(sortInForce(filters)).toBe('cost');
	});

	it('drops what the API would refuse', () => {
		// A parameter given without a value is a 400 on purpose (spec 004 #23).
		expect(userSearch({ sort: '', prefix: '   ' })).toBe('');
		expect(readUserFilters(new URLSearchParams('prefix='))).toEqual({});
	});

	it('falls back to the default sort rather than sending a 400', () => {
		// The URL is hand-editable, and a listing is not the place to argue
		// about a typo.
		expect(readUserFilters(new URLSearchParams('sort=oldest'))).toEqual({});
		expect(sortInForce({})).toBe(DEFAULT_USER_SORT);
	});
});

describe('the user page in the URL', () => {
	it('names the tab, and defaults to sessions', () => {
		expect(readTab(new URLSearchParams('tab=traces'))).toBe('traces');
		expect(readTab(new URLSearchParams('tab=nonsense'))).toBe('sessions');
		expect(readTab(new URLSearchParams())).toBe('sessions');
	});

	it('starts the tab’s listing over and closes the panel on a tab change', () => {
		const at = new URLSearchParams('tab=sessions&peek=s-1&cursor=abc&limit=100&from=X');

		const search = userPageSearch(at, { tab: 'traces' });
		const next = new URLSearchParams(search);
		expect(next.get('tab')).toBe('traces');
		// A cursor is a position in one listing and means nothing in another.
		expect(next.get('cursor')).toBeNull();
		expect(next.get('peek')).toBeNull();
		// The window and the page size are preferences and travel.
		expect(next.get('from')).toBe('X');
		expect(next.get('limit')).toBe('100');
	});

	it('keeps the panel when only the window moves, and clears a window on empty', () => {
		const at = new URLSearchParams('tab=traces&peek=t-1&from=A&to=B');

		const moved = new URLSearchParams(userPageSearch(at, { from: 'C', to: '' }));
		expect(moved.get('from')).toBe('C');
		expect(moved.get('to')).toBeNull();
		expect(moved.get('tab')).toBe('traces');
		expect(moved.get('peek')).toBe('t-1');
	});
});
