<script lang="ts">
	import type { ScoreConfig, ScoreSeries } from '$lib/api/client.svelte';
	import { axisRange, buildScoreSeries, figure } from '$lib/api/quality';
	import type { Bucket } from '$lib/api/range';
	import Chart from '$lib/components/Chart.svelte';
	import { count } from '$lib/format';
	import { href } from '$lib/project.svelte';

	// The Quality overview's cards (spec 025 #10), one component so the
	// dashboard draws the same card (spec 034 #5): the mean or the rate as a
	// small chart over the window, leading to that score's detail. The caller
	// says which series, in what order, and whether there is more to see.

	let {
		series,
		configs,
		window,
		more = 0
	}: {
		series: ScoreSeries[];
		configs: ScoreConfig[];
		window: { from?: string; to?: string; bucket: Bucket; now: Date };
		/** How many score names are not drawn here; a *See all* link says so. */
		more?: number;
	} = $props();

	const configOf = (one: ScoreSeries) => configs.find((config) => config.name === one.name);

	/** How a value reads in a legend, by the series' type. */
	function formatter(one: ScoreSeries) {
		if (one.data_type === 'numeric')
			return (value: number | null | undefined) => (typeof value === 'number' ? figure(value) : '—');
		return (value: number | null | undefined) =>
			typeof value === 'number' ? `${Math.round(value * 100)}%` : '—';
	}
</script>

<div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
	{#each series as one (one.name + one.data_type)}
		{@const shape = buildScoreSeries(one, window)}
		<a
			href={href('/quality', new URLSearchParams({ name: one.name }))}
			title={configOf(one)?.description || undefined}
			class="focus-visible:outline-accent block rounded-lg transition-opacity duration-100 hover:opacity-80"
		>
			<Chart
				title="{one.name} · {one.data_type} · {count(shape.total)} scores"
				x={shape.x}
				lines={shape.primary}
				format={formatter(one)}
				range={axisRange(configOf(one))}
				summary="{one.name} per {window.bucket} over {count(shape.total)} scores. Open for the breakdowns."
				sync="quality-card-{one.name}-{one.data_type}"
				height={120}
			/>
		</a>
	{/each}
</div>
{#if more > 0}
	<p class="mt-2 text-sm">
		<a href={href('/quality')} class="text-accent hover:underline">
			See all {count(series.length + more)} score names
		</a>
	</p>
{/if}
