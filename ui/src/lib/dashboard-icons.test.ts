import { describe, expect, it } from 'vitest';
import { BLOCKS } from './dashboard';
import { BLOCK_ICONS } from './dashboard-icons';
import { ICONS } from './sections';

describe('the dashboard icons (spec 034 #16)', () => {
	it('gives every block but the summary one icon, and no two blocks the same', () => {
		const ids = BLOCKS.map((block) => block.id).filter((id) => id !== 'summary');
		expect(Object.keys(BLOCK_ICONS).sort()).toEqual([...ids].sort());
		expect(new Set(Object.values(BLOCK_ICONS)).size).toBe(ids.length);
	});

	it('takes Traces and Quality from the sidebar, so a screen and its figure agree', () => {
		expect(BLOCK_ICONS.traces).toBe(ICONS['/traces']);
		expect(BLOCK_ICONS.quality).toBe(ICONS['/quality']);
	});
});
