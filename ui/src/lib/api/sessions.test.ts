import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { SESSION_FILTERS, readSessionFilters, sessionSearch } from './sessions';

/**
 * The same parity idea the trace filters are held to, pointed at the endpoint
 * spec 007 added: the document the server serves is read here, and a filter it
 * accepts that the screen does not offer — or the other way round — fails.
 */
function documentedFilters(): string[] {
	const document = JSON.parse(
		readFileSync(resolve(process.cwd(), '../internal/server/openapi.json'), 'utf8')
	);
	const parameters = document.paths['/api/v1/sessions'].get.parameters as Array<
		{ name: string } | { $ref: string }
	>;
	const plumbing = new Set(['limit', 'cursor']);
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
		expect([...SESSION_FILTERS].sort()).toEqual(documentedFilters().sort());
	});
});

describe('filters in the URL', () => {
	it('round-trips through a query string', () => {
		const filters = readSessionFilters(
			new URLSearchParams(
				'environment=prod&user_id=u1&from=2026-09-01T00:00:00Z&to=2026-09-02T00:00:00Z'
			)
		);

		expect(filters).toEqual({
			environment: 'prod',
			user_id: 'u1',
			from: '2026-09-01T00:00:00Z',
			to: '2026-09-02T00:00:00Z'
		});
		expect(sessionSearch(filters)).toBe(
			'?from=2026-09-01T00%3A00%3A00Z&to=2026-09-02T00%3A00%3A00Z&environment=prod&user_id=u1'
		);
	});

	it('drops what the API would refuse', () => {
		// A parameter given without a value is a 400 on purpose (spec 004 #23).
		expect(sessionSearch({ environment: '   ', user_id: '' })).toBe('');
		expect(readSessionFilters(new URLSearchParams('environment='))).toEqual({});
	});

});
