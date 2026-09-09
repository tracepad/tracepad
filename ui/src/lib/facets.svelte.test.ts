import { flushSync } from 'svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { FacetValues } from './facets.svelte';

// The one read behind every facet list (spec 027 #6): what it holds while a
// read is in flight, and what it forgets between openings.

const getFacets = vi.fn();

vi.mock('./api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: { getFacets: (...args: unknown[]) => getFacets(...args) }
}));

const answer = (environments: [string, number][]) => ({
	from: 'a',
	to: 'b',
	environment: environments.map(([value, count]) => ({ value, count })),
	release: [],
	name: [],
	omitted: { environment: 0, release: 0, name: 0 }
});

/** A read that only lands when the test says so. */
function deferred() {
	let land: (value: unknown) => void = () => {};
	const promise = new Promise((resolve) => (land = resolve));
	return { promise, land };
}

beforeEach(() => {
	getFacets.mockReset();
});

describe('a read in flight', () => {
	// The regression: moving the range with the panel open left the previous
	// window's values and counts on screen until the answer landed. A count
	// for a range that is no longer on screen is worse than no count at all,
	// because nothing says it is the wrong one — and #6 says the checked
	// values render alone while loading.
	it('shows nothing from the window it is leaving', async () => {
		const first = deferred();
		const second = deferred();
		getFacets.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);

		let window = $state<{ from?: string; to?: string }>({});
		const open = $state({ now: true });
		const facets = new FacetValues(
			() => window,
			() => open.now
		);
		const stop = $effect.root(() => {
			$effect(() => facets.watch());
		});
		flushSync();

		first.land(answer([['production', 40]]));
		await first.promise;
		expect(facets.values.environment).toHaveLength(1);

		// The range moves under the open panel.
		window = { from: '2026-09-01T00:00:00Z' };
		flushSync();
		expect(facets.loading).toBe(true);
		expect(facets.values.environment).toEqual([]);
		expect(facets.omitted.environment).toBe(0);

		second.land(answer([['staging', 3]]));
		await second.promise;
		await Promise.resolve();
		expect(facets.values.environment).toEqual([{ value: 'staging', count: 3 }]);

		stop();
	});
});
