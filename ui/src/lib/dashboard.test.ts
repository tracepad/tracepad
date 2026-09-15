import { describe, expect, it } from 'vitest';
import {
	arrangementOf,
	BLOCKS,
	DEFAULT_ARRANGEMENT,
	reordered,
	withArrangement,
	type BlockId
} from './dashboard';

// The arrangement is read out of an opaque object and written back into it
// (spec 034 #8, #9). What has to hold: a block a later spec adds appears
// without a migration, a value a newer build wrote does not crash this one,
// and a key another screen keeps survives the round trip.

const P = 'a'.repeat(32);
const ALL = BLOCKS.map((block) => block.id);

describe('arrangementOf', () => {
	it('is the default for an account that kept nothing', () => {
		expect(arrangementOf({}, P)).toEqual(DEFAULT_ARRANGEMENT);
		expect(arrangementOf({ dashboard: { other: { order: ['errors'] } } }, P)).toEqual(DEFAULT_ARRANGEMENT);
	});

	it('drops unknown ids and appends the missing ones in default order', () => {
		const stored = { dashboard: { [P]: { order: ['errors', 'sparklines', 'summary'], hidden: ['tokens', 'weather'] } } };
		const { order, hidden } = arrangementOf(stored, P);

		expect(order.slice(0, 2)).toEqual(['errors', 'summary']);
		expect(order).toEqual(['errors', 'summary', ...ALL.filter((id) => id !== 'errors' && id !== 'summary')]);
		expect(hidden).toEqual(['tokens']);
	});

	it('survives a value that is not an arrangement at all', () => {
		for (const broken of [
			{ dashboard: 'errors' },
			{ dashboard: { [P]: 'errors' } },
			{ dashboard: { [P]: { order: 'errors', hidden: 7 } } },
			{ dashboard: { [P]: { order: ['errors', 'errors', 3, null] } } }
		]) {
			const { order, hidden } = arrangementOf(broken, P);
			expect(new Set(order)).toEqual(new Set(ALL));
			expect(order).toHaveLength(ALL.length);
			expect(hidden).toEqual([]);
		}
	});
});

describe('withArrangement', () => {
	it('replaces the project key and keeps everything else', () => {
		const before = { theme: 'dark', dashboard: { other: { order: ['cost'] } } };
		const next = withArrangement(before, P, { order: ['errors'] as BlockId[], hidden: ['tokens'] });

		expect(next).toEqual({
			theme: 'dark',
			dashboard: { other: { order: ['cost'] }, [P]: { order: ['errors'], hidden: ['tokens'] } }
		});
		expect(before.dashboard).not.toHaveProperty(P);
	});

	it('deletes the project key on reset and leaves the neighbours', () => {
		const before = { dashboard: { other: { order: ['cost'] }, [P]: { order: ['errors'] } } };

		expect(withArrangement(before, P, null)).toEqual({ dashboard: { other: { order: ['cost'] } } });
	});
});

describe('reordered', () => {
	it('places the dropped order around the hidden blocks, which keep their slots', () => {
		const arrangement = { order: [...ALL], hidden: ['tokens', 'models'] as BlockId[] };
		const visible = ALL.filter((id) => !arrangement.hidden.includes(id));
		// Errors dragged to the top.
		const dropped: BlockId[] = ['errors', ...visible.filter((id) => id !== 'errors')];

		expect(reordered(arrangement, dropped).order).toEqual([
			'errors', 'summary', 'traces', 'tokens', 'cost', 'latency', 'models', 'environments', 'releases', 'quality'
		]);
	});
});
