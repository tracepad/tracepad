<script lang="ts" module>
	import type { Observation } from '$lib/api/client.svelte';

	/** One observation as the tree draws it. */
	export type TreeRow = {
		observation: Observation;
		depth: number;
		parentID: string | null;
		expandable: boolean;
		/** This observation failed, or something under it did. */
		failing: boolean;
	};

	/**
	 * Flattens the tree into the rows that are actually visible, which is what
	 * makes arrow-key walking a matter of moving one index.
	 *
	 * A failure is carried up as it goes: a parent whose descendant failed is
	 * marked failing too, so collapsing a subtree never hides that something
	 * inside it went wrong (Application contract).
	 */
	export function flatten(
		observations: Observation[],
		collapsed: ReadonlySet<string>
	): TreeRow[] {
		const rows: TreeRow[] = [];
		const walk = (nodes: Observation[], depth: number, parentID: string | null): boolean => {
			let failed = false;
			for (const observation of nodes) {
				const children = observation.children ?? [];
				const row: TreeRow = {
					observation,
					depth,
					parentID,
					expandable: children.length > 0,
					failing: observation.level === 'ERROR'
				};
				rows.push(row);
				const index = rows.length - 1;
				const hidden = collapsed.has(observation.id);
				// The subtree is walked even when it is collapsed: its rows are
				// dropped, but whether it failed still has to travel upwards.
				const before = rows.length;
				const childFailed = walk(children, depth + 1, observation.id);
				if (hidden) rows.length = before;
				rows[index] = { ...row, failing: row.failing || childFailed };
				failed ||= rows[index].failing;
			}
			return failed;
		};
		walk(observations, 0, null);
		return rows;
	}

	/** Three letters of vertical rhythm instead of a word of ragged width. */
	export const TYPE_LABELS: Record<string, string> = {
		span: 'SPAN',
		generation: 'GEN',
		event: 'EVENT'
	};
</script>

<script lang="ts">
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { SvelteSet } from 'svelte/reactivity';
	import { cost, duration, elapsed } from '$lib/format';

	// The observation tree (design §8, hand-written). Keyboard behaviour
	// follows the ARIA tree pattern: one tab stop, arrows walk it, left and
	// right close and open (accessibility floor).

	let {
		observations,
		selectedID,
		onselect
	}: {
		observations: Observation[];
		selectedID: string | null;
		/**
		 * `activate` separates "the highlight moved" from "open this one".
		 * They are the same thing on a wide screen, where both panes are
		 * visible; on a phone, where the detail replaces the tree, walking
		 * with the arrows must not throw the reader out of the tree.
		 */
		onselect: (id: string, activate: boolean) => void;
	} = $props();

	const collapsed = new SvelteSet<string>();
	const rows = $derived(flatten(observations, collapsed));
	// The tab stop: the selection, or the first row when nothing is selected.
	const activeID = $derived(
		rows.some((row) => row.observation.id === selectedID) ? selectedID : rows[0]?.observation.id
	);

	let elements: Record<string, HTMLElement | null> = {};

	function select(id: string | undefined, activate = false) {
		if (!id) return;
		onselect(id, activate);
		elements[id]?.focus();
	}

	function toggle(id: string, open: boolean) {
		if (open) collapsed.delete(id);
		else collapsed.add(id);
	}

	function onkeydown(event: KeyboardEvent) {
		const index = rows.findIndex((row) => row.observation.id === activeID);
		if (index < 0) return;
		const row = rows[index];
		const open = !collapsed.has(row.observation.id);
		switch (event.key) {
			case 'ArrowDown':
				select(rows[index + 1]?.observation.id);
				break;
			case 'ArrowUp':
				select(rows[index - 1]?.observation.id);
				break;
			case 'ArrowRight':
				if (row.expandable && !open) toggle(row.observation.id, true);
				else if (row.expandable) select(rows[index + 1]?.observation.id);
				else return;
				break;
			case 'ArrowLeft':
				if (row.expandable && open) toggle(row.observation.id, false);
				else if (row.parentID) select(row.parentID);
				else return;
				break;
			case 'Home':
				select(rows[0]?.observation.id);
				break;
			case 'End':
				select(rows[rows.length - 1]?.observation.id);
				break;
			default:
				return;
		}
		event.preventDefault();
	}

	const nodeCost = (observation: Observation) => {
		const total = (observation.cost_details as Record<string, unknown> | undefined)?.total;
		return typeof total === 'number' ? total : null;
	};
</script>

<!-- The tab stop lives on the active treeitem, per the ARIA tree pattern; the
     container takes -1 so that a click on the padding does not lose focus. -->
<div
	role="tree"
	aria-label="Observations"
	tabindex="-1"
	{onkeydown}
	class="min-h-0 flex-1 overflow-auto py-1"
>
	{#each rows as row (row.observation.id)}
		{@const id = row.observation.id}
		{@const open = !collapsed.has(id)}
		{@const selected = id === selectedID}
		<div
			role="treeitem"
			aria-level={row.depth + 1}
			aria-selected={selected}
			aria-expanded={row.expandable ? open : undefined}
			tabindex={id === activeID ? 0 : -1}
			bind:this={elements[id]}
			onclick={() => select(id, true)}
			onkeydown={(event) => {
				if (event.key === 'Enter' || event.key === ' ') {
					event.preventDefault();
					select(id, true);
				}
			}}
			style:padding-left="{row.depth * 0.875 + 0.25}rem"
			class={[
				'pointer-coarse:min-h-11 flex cursor-pointer items-center gap-1.5 rounded px-1 py-1',
				'transition-colors duration-100',
				selected ? 'bg-accent-soft' : 'hover:bg-raised'
			]}
		>
			{#if row.expandable}
				<button
					type="button"
					tabindex="-1"
					aria-label={open ? 'Collapse' : 'Expand'}
					onclick={(event) => {
						event.stopPropagation();
						toggle(id, !open);
					}}
					class="text-subtle hover:text-fg -m-1 cursor-pointer p-1"
				>
					{#if open}<ChevronDown class="size-3.5" />{:else}<ChevronRight class="size-3.5" />{/if}
				</button>
			{:else}
				<span class="w-3.5 shrink-0"></span>
			{/if}

			<span
				class="border-border text-subtle shrink-0 rounded border px-1 font-mono text-[10px]
					uppercase"
			>
				{TYPE_LABELS[row.observation.type] ?? row.observation.type}
			</span>

			<span class="min-w-0 flex-1 truncate" title={row.observation.name ?? id}>
				{row.observation.name ?? id}
			</span>

			{#if row.failing}
				<TriangleAlert
					class="text-danger size-3.5 shrink-0"
					aria-label={row.observation.level === 'ERROR'
						? 'Failed'
						: 'Something inside this failed'}
				/>
			{/if}
			{#if nodeCost(row.observation) !== null}
				<span class="text-subtle shrink-0 font-mono text-xs tabular-nums">
					{cost(nodeCost(row.observation))}
				</span>
			{/if}
			<span class="text-muted w-14 shrink-0 text-right font-mono text-xs tabular-nums">
				{duration(elapsed(row.observation.start_time, row.observation.end_time))}
			</span>
		</div>
	{/each}
</div>
