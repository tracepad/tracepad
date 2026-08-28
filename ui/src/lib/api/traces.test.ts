import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import type { TraceRow } from './client.svelte';
import { TRACE_FILTERS, filterCount, filterSearch, mergeRows, readFilters } from './traces';

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
	const plumbing = new Set(['fields', 'limit', 'cursor']);
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
					'session_id=s1&name=chat&from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z'
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
			to: '2026-08-02T00:00:00Z'
		});
		expect(filterCount(filters)).toBe(9);
		expect(readFilters(new URLSearchParams(filterSearch(filters)))).toEqual(filters);
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

const row = (id: string, timestamp?: string, extra: Partial<TraceRow> = {}): TraceRow => ({
	id,
	timestamp,
	environment: 'default',
	error_count: 0,
	observation_count: 1,
	...extra
});

describe('merging a live poll into the rows on screen', () => {
	it('keeps a trace seen twice once, newest first', () => {
		const onScreen = [row('bb', '2026-08-28T12:00:02Z'), row('aa', '2026-08-28T12:00:01Z')];
		const polled = [row('cc', '2026-08-28T12:00:03Z'), row('bb', '2026-08-28T12:00:02Z')];

		const merged = mergeRows(onScreen, polled);

		expect(merged.map((r) => r.id)).toEqual(['cc', 'bb', 'aa']);
	});

	it('puts a late arrival where the server would have put it', () => {
		const onScreen = [row('cc', '2026-08-28T12:00:03Z'), row('aa', '2026-08-28T12:00:01Z')];
		const polled = [row('bb', '2026-08-28T12:00:02Z')];

		expect(mergeRows(onScreen, polled).map((r) => r.id)).toEqual(['cc', 'bb', 'aa']);
	});

	it('takes the fresher copy of a trace that is still being written', () => {
		const onScreen = [row('aa', '2026-08-28T12:00:01Z', { observation_count: 1 })];
		const polled = [row('aa', '2026-08-28T12:00:01Z', { observation_count: 7, error_count: 1 })];

		expect(mergeRows(onScreen, polled)[0].observation_count).toBe(7);
	});

	it('orders a sub-second fraction as an instant, not as text', () => {
		// `.` sorts before `Z`, so comparing the RFC 3339 strings directly
		// would put 12:00:00.5 before 12:00:00.
		const merged = mergeRows(
			[],
			[row('aa', '2026-08-28T12:00:00Z'), row('bb', '2026-08-28T12:00:00.5Z')]
		);

		expect(merged.map((r) => r.id)).toEqual(['bb', 'aa']);
	});

	it('breaks a tie the way the listing does', () => {
		const merged = mergeRows(
			[],
			[row('aa', '2026-08-28T12:00:00Z'), row('ff', '2026-08-28T12:00:00Z')]
		);

		// ORDER BY timestamp DESC, id DESC.
		expect(merged.map((r) => r.id)).toEqual(['ff', 'aa']);
	});

	it('folds a cursor page in without repeating what a poll already pulled', () => {
		// Live mode and "load more" overlap: a new arrival shifts the whole
		// cursor window down by one, so the next page re-delivers the row the
		// poll has already merged. A keyed each-block throws on a repeated id,
		// which is why the page is merged rather than appended.
		const onScreen = [
			row('dd', '2026-08-28T12:00:04Z'), // arrived via a live tick
			row('cc', '2026-08-28T12:00:03Z'),
			row('bb', '2026-08-28T12:00:02Z')
		];
		const nextPage = [row('bb', '2026-08-28T12:00:02Z'), row('aa', '2026-08-28T12:00:01Z')];

		const merged = mergeRows(onScreen, nextPage);

		expect(merged.map((r) => r.id)).toEqual(['dd', 'cc', 'bb', 'aa']);
		expect(new Set(merged.map((r) => r.id)).size).toBe(merged.length);
	});

	it('sorts a trace with no timestamp last rather than first', () => {
		const merged = mergeRows([], [row('aa'), row('bb', '2026-08-28T12:00:00Z')]);

		expect(merged.map((r) => r.id)).toEqual(['bb', 'aa']);
	});
});
