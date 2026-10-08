<script lang="ts">
	import ArrowDownRight from '@lucide/svelte/icons/arrow-down-right';
	import ArrowUpRight from '@lucide/svelte/icons/arrow-up-right';
	import Minus from '@lucide/svelte/icons/minus';
	import type { SummaryFigure } from '$lib/api/stats';
	import BlockIcon from './BlockIcon.svelte';

	// One tile of the summary row (spec 034 #2): the label, the figure large
	// in tabular numerals, and the change under it with its glyph and colour.
	// While the figures load the tile keeps its height with a placeholder,
	// so the charts below do not jump when the numbers arrive.

	let { figure, loading = false }: { figure: SummaryFigure; loading?: boolean } = $props();

	const Glyph = $derived(
		figure.direction === 'up' ? ArrowUpRight : figure.direction === 'down' ? ArrowDownRight : Minus
	);
	const tint = $derived(
		figure.tone === 'worse' ? 'text-danger' : figure.tone === 'better' ? 'text-ok' : 'text-muted'
	);
</script>

<div
	class="border-border bg-surface min-w-0 rounded-lg border px-3 py-2"
	title={figure.previous === null ? undefined : `Previous window: ${figure.previous}`}
>
	<p class="text-subtle flex items-center gap-1.5 text-xs font-medium">
		<BlockIcon id={figure.id} alert={figure.id === 'errors' && figure.positive} />
		{figure.label}
	</p>
	{#if loading}
		<p class="text-subtle text-2xl leading-tight font-semibold tabular-nums" aria-busy="true">…</p>
		<p class="text-subtle h-5 text-xs">&nbsp;</p>
	{:else}
		<p class="text-2xl leading-tight font-semibold tabular-nums">{figure.value}</p>
		<p class="{tint} flex h-5 items-center gap-0.5 text-xs tabular-nums">
			{#if figure.change !== null}
				{#if figure.direction}<Glyph class="size-3.5" aria-hidden="true" />{/if}
				{figure.change}
				{#if figure.direction}<span class="sr-only">against the previous window</span>{/if}
			{/if}
		</p>
	{/if}
</div>
