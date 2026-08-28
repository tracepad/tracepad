import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import type { Observation } from '$lib/api/client.svelte';
import TraceTree, { flatten } from './TraceTree.svelte';

const node = (id: string, extra: Partial<Observation> = {}): Observation => ({
	id,
	type: 'span',
	name: id,
	level: 'DEFAULT',
	...extra
});

/** root ▸ a ▸ a1 (failing) ; root ▸ b */
const tree: Observation[] = [
	node('root', {
		children: [node('a', { children: [node('a1', { level: 'ERROR' })] }), node('b')]
	})
];

describe('flattening the tree', () => {
	it('walks depth first, carrying the depth of each node', () => {
		expect(flatten(tree, new Set()).map((row) => [row.observation.id, row.depth])).toEqual([
			['root', 0],
			['a', 1],
			['a1', 2],
			['b', 1]
		]);
	});

	it('hides the children of a collapsed node', () => {
		const rows = flatten(tree, new Set(['a']));

		expect(rows.map((row) => row.observation.id)).toEqual(['root', 'a', 'b']);
	});

	it('shows a failure through the ancestors that could hide it', () => {
		// The point of the rule: collapsing a subtree must not make a failure
		// inside it disappear from the screen.
		const rows = flatten(tree, new Set(['a']));
		const failing = Object.fromEntries(rows.map((row) => [row.observation.id, row.failing]));

		expect(failing).toEqual({ root: true, a: true, b: false });
	});

	it('marks a leaf that failed and nothing else', () => {
		const rows = flatten([node('x'), node('y', { level: 'ERROR' })], new Set());

		expect(rows.map((row) => row.failing)).toEqual([false, true]);
	});

	it('survives a tree deep enough to be a problem', () => {
		let deep = node('leaf-999', { level: 'ERROR' });
		for (let level = 998; level >= 0; level--) deep = node(`leaf-${level}`, { children: [deep] });

		const rows = flatten([deep], new Set());

		expect(rows).toHaveLength(1000);
		expect(rows[999].depth).toBe(999);
		// The failure at the bottom reaches the top.
		expect(rows[0].failing).toBe(true);
	});
});

describe('walking the tree with the keyboard', () => {
	const setup = () => {
		const onselect = vi.fn();
		render(TraceTree, { observations: tree, selectedID: 'root', onselect });
		return { onselect, user: userEvent.setup() };
	};

	it('moves the selection down and up', async () => {
		const { onselect, user } = setup();
		screen.getByRole('treeitem', { selected: true }).focus();

		await user.keyboard('{ArrowDown}');

		expect(onselect).toHaveBeenLastCalledWith('a');
	});

	it('closes a branch with the left arrow before leaving it', async () => {
		const { onselect, user } = setup();
		screen.getByRole('treeitem', { selected: true }).focus();

		await user.keyboard('{ArrowLeft}');

		// The root is expanded, so the first press collapses rather than moves.
		expect(onselect).not.toHaveBeenCalled();
		expect(screen.getAllByRole('treeitem')).toHaveLength(1);
	});

	it('opens a closed branch with the right arrow', async () => {
		const { user } = setup();
		screen.getByRole('treeitem', { selected: true }).focus();

		await user.keyboard('{ArrowLeft}{ArrowRight}');

		expect(screen.getAllByRole('treeitem')).toHaveLength(4);
	});

	it('jumps to the last visible row with End', async () => {
		const { onselect, user } = setup();
		screen.getByRole('treeitem', { selected: true }).focus();

		await user.keyboard('{End}');

		expect(onselect).toHaveBeenLastCalledWith('b');
	});

	it('keeps exactly one tab stop', () => {
		setup();

		const stops = screen
			.getAllByRole('treeitem')
			.filter((item) => item.getAttribute('tabindex') === '0');
		expect(stops).toHaveLength(1);
	});
});
