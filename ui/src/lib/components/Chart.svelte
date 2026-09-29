<script lang="ts">
	import uPlot from 'uplot';
	import 'uplot/dist/uPlot.min.css';
	import { hasData, drawn, type Line } from '$lib/chart';
	import { theme } from '$lib/theme.svelte';

	// The one chart component (spec 007 #6). uPlot draws the four time series —
	// it is 40 KB, it is built for exactly this, and it is the only charting
	// dependency in the tree; the categorical breakdowns are tables with CSS
	// bars, because category bars here would mean hand-built paths and long
	// model names crushed into an axis.
	//
	// It is themed with our own tokens rather than uPlot's defaults. A token is
	// a `light-dark()` pair, and a custom property's computed value is the
	// unresolved pair — so the colours are read off a probe element, which is
	// the only thing that resolves them the way the rest of the page does.


	let {
		title,
		x,
		lines,
		format,
		summary,
		height = 150,
		range,
		sync = 'tracepad-stats'
	}: {
		title: string;
		/** Bucket starts, seconds since the epoch. */
		x: number[];
		lines: Line[];
		/** How a value reads in the legend and on the y axis. */
		format: (value: number | null | undefined) => string;
		/** What the chart says, for somebody who cannot see it. */
		summary: string;
		height?: number;
		/**
		 * A y axis pinned to these bounds instead of to the data's own. A
		 * score config that declares `min` and `max` is a claim about the
		 * scale the values live on, and drawing 0.2..0.3 across the full
		 * height would make a stable score look like a cliff (spec 025 #10).
		 */
		range?: [number, number];
		/**
		 * Which group of charts share the x cursor. Charts of one question are
		 * stacked and read together — reading "cost at 14:00" off the second
		 * while the first says 14:00 is the whole reason they are stacked — so
		 * they take one key. Cards about *different* scores are not one
		 * question, and each takes a key of its own; a cursor moving on six of
		 * them at once is a cursor nobody put there.
		 *
		 * The default is the Stats screen's group, which is where every chart
		 * written before this prop existed belongs.
		 */
		sync?: string;
	} = $props();

	/**
	 * Whether the system is asking for a dark page. Read reactively because the
	 * palette depends on it whenever the theme setting is `system`, and a
	 * chart is drawn onto a canvas — nothing about it re-styles itself when the
	 * scheme flips underneath.
	 */
	let systemDark = $state(false);

	$effect(() => {
		const query = window.matchMedia('(prefers-color-scheme: dark)');
		systemDark = query.matches;
		const follow = () => (systemDark = query.matches);
		query.addEventListener('change', follow);
		return () => query.removeEventListener('change', follow);
	});

	/** Resolves `--color-x` the way an element using it would. */
	function palette(root: HTMLElement, names: string[]): Record<string, string> {
		const probe = document.createElement('span');
		probe.style.cssText = 'position:absolute;visibility:hidden;pointer-events:none';
		root.appendChild(probe);
		const colours: Record<string, string> = {};
		for (const name of names) {
			probe.style.color = `var(--color-${name})`;
			colours[name] = getComputedStyle(probe).color;
		}
		probe.remove();
		return colours;
	}

	/** What both axes share: our own ink, and the type scale of the page. */
	function axis(colours: Record<string, string>) {
		return {
			stroke: colours.muted,
			grid: { stroke: colours.border, width: 1 },
			ticks: { stroke: colours.border },
			font: '10px ui-monospace, monospace'
		};
	}

	/**
	 * Whether there is anything to draw. An axis exists for any window — time
	 * passed — but a chart of nothing but gaps is an empty frame, and an empty
	 * frame is worse than a sentence saying the window is empty.
	 */
	const populated = $derived(x.length > 0 && lines.some(hasData));

	/**
	 * What the reader clicked in the legend, by label. The chart is rebuilt
	 * whenever the theme, the range or the data moves, and a rebuilt chart
	 * that forgot its toggles would put back what they had just hidden. Not
	 * reactive on purpose: a click must not itself rebuild the chart.
	 */
	const chosen = new Map<string, boolean>();

	function draw(node: HTMLDivElement) {
		// Reading these here is what redraws the chart when the theme moves.
		theme.value;
		systemDark;
		const colours = palette(node, ['muted', 'border', ...lines.map((line) => line.token)]);

		const chart = new uPlot(
			{
				title,
				width: node.clientWidth || 320,
				height,
				padding: [8, 8, 0, 0],
				// One x cursor across the four charts: reading "cost at 14:00"
				// off the second chart while the first says 14:00 is the whole
				// reason they are stacked.
				cursor: {
					sync: { key: sync, scales: ['x', null] },
					drag: { x: false, y: false }
				},
				...(range ? { scales: { y: { range } } } : {}),
				legend: { live: true },
				hooks: {
					setSeries: [
						// Only a toggle: the same hook fires for a focus change.
						(chart, index, opts) => {
							const line = index === null ? undefined : lines[index - 1];
							if (line && opts && 'show' in opts) chosen.set(line.label, !!chart.series[index!].show);
						}
					]
				},
				axes: [
					{ ...axis(colours), space: 64 },
					{
						...axis(colours),
						// Wide enough for the longest label this formatter
						// produces — a cost axis reads `$0.0100`, and an axis
						// whose first digit is cut off is worse than no axis.
						size: 68,
						values: (_, ticks) => ticks.map((tick) => format(tick))
					}
				],
				series: [
					{ label: 'Time' },
					...lines.map((line) => ({
						label: line.label,
						show: drawn(lines, chosen, line),
						stroke: colours[line.token],
						width: 1.5,
						points: { show: x.length < 40 },
						// A bucket the server did not return is a gap, never a
						// zero: uPlot leaves a null alone, and so do we.
						value: (_: uPlot, value: number | null) => format(value)
					}))
				]
			},
			[x, ...lines.map((line) => line.values)] as uPlot.AlignedData,
			node
		);

		const observer = new ResizeObserver(() => chart.setSize({ width: node.clientWidth, height }));
		observer.observe(node);
		return () => {
			observer.disconnect();
			chart.destroy();
		};
	}
</script>

<figure class="border-border bg-surface min-w-0 rounded-lg border p-2">
	{#if populated}
		<!-- The canvas is decoration to a screen reader; the sentence below it
		     is the chart. The breakdown tables carry the same numbers per
		     category, which is the tabular alternative for the rest. -->
		<div class="chart w-full" aria-hidden="true" {@attach draw}></div>
		<figcaption class="sr-only">{title}. {summary}</figcaption>
	{:else}
		<figcaption class="text-subtle px-1 py-0.5 text-xs font-medium">{title}</figcaption>
		<p class="text-subtle flex items-center justify-center px-2 text-sm" style:height="{height}px">
			Nothing in this window
		</p>
	{/if}
</figure>

<style>
	/* uPlot renders its own title and legend as plain DOM; these are the four
	 * declarations it takes to make them look like the rest of the app. */
	.chart :global(.u-title) {
		font-size: var(--text-xs);
		font-weight: 500;
		color: var(--color-subtle);
		text-align: left;
		padding-left: 4px;
	}
	.chart :global(.u-legend) {
		font-size: var(--text-xs);
		color: var(--color-muted);
	}
	.chart :global(.u-legend .u-marker) {
		border-width: 2px;
	}
	.chart :global(.u-select) {
		background: var(--color-accent-soft);
	}
</style>
