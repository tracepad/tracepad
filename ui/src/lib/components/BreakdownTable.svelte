<script lang="ts">
	import type { BreakdownRow } from '$lib/api/stats';
	import { cost, count } from '$lib/format';

	// The categorical half of Stats (spec 007 #6): a table with proportion
	// bars, not a second chart library. Model names get long, and a table row
	// is where a long label reads well.
	//
	// The bar is drawn against the largest row rather than against the total —
	// "which of these is the big one" is the question a breakdown answers, and
	// a share of the total renders every row of a twenty-model deployment as
	// the same invisible sliver. The number beside it is the absolute value, so
	// nothing here is only a proportion.

	let {
		title,
		unit,
		rows,
		label,
		tokens = false
	}: {
		title: string;
		/** What `count` counts. The API says so, and so does the header. */
		unit: string;
		rows: BreakdownRow[];
		/** What the first column holds, e.g. "Model". */
		label: string;
		/**
		 * Whether to draw the Tokens column. On for the Stats screen and off
		 * for a user's page, whose answer never carries tokens (spec 031
		 * #11): a column of dashes would be noise, not information.
		 */
		tokens?: boolean;
	} = $props();

	const cells = $derived([
		{ share: (row: BreakdownRow) => row.countShare, text: (row: BreakdownRow) => count(row.count), tint: 'bg-accent' },
		{ share: (row: BreakdownRow) => row.errorShare, text: (row: BreakdownRow) => count(row.errorCount), tint: 'bg-danger' },
		{ share: (row: BreakdownRow) => row.costShare, text: (row: BreakdownRow) => cost(row.cost), tint: 'bg-accent' },
		...(tokens
			? [{ share: (row: BreakdownRow) => row.tokensShare, text: (row: BreakdownRow) => count(row.tokens), tint: 'bg-accent' }]
			: [])
	]);
</script>

<section class="border-border bg-surface min-w-0 rounded-lg border">
	<h2 class="text-subtle border-border border-b px-3 py-2 text-xs font-medium">{title}</h2>
	{#if rows.length === 0}
		<p class="text-subtle px-3 py-6 text-center text-sm">Nothing in this window</p>
	{:else}
		<div class="overflow-x-auto">
			<table class="w-full min-w-lg border-collapse text-left">
				<thead class="text-subtle text-xs whitespace-nowrap">
					<tr class="border-border border-b">
						<th scope="col" class="px-3 py-1.5 font-medium">{label}</th>
						<th scope="col" class="w-44 px-3 py-1.5 font-medium">
							{unit === 'observation' ? 'Observations' : 'Traces'}
						</th>
						<th scope="col" class="w-36 px-3 py-1.5 font-medium">Errors</th>
						<th scope="col" class="w-36 px-3 py-1.5 font-medium">Cost</th>
						{#if tokens}
							<!-- Input plus output: what a bill is made of. One number
							     rather than a column per class, which would widen the
							     table past a phone (spec 031 #6). -->
							<th scope="col" class="w-36 px-3 py-1.5 font-medium">Tokens</th>
						{/if}
					</tr>
				</thead>
				<tbody>
					{#each rows as row (row.key)}
						<tr class="border-border border-b last:border-b-0">
							<th
								scope="row"
								class="max-w-64 truncate px-3 py-1.5 text-left font-mono text-xs font-normal"
								title={row.key}
							>
								{row.key}
							</th>
							{#each cells as cell, index (index)}
								<td class="px-3 py-1.5">
									<!-- The bar sits behind the number rather than beside
									     it, so a long column does not push the figures out
									     of alignment. -->
									<div class="relative">
										<div
											class="{cell.tint} absolute inset-y-0 left-0 rounded-sm opacity-20"
											style:width="{Math.round(cell.share(row) * 100)}%"
										></div>
										<span class="relative tabular-nums">{cell.text(row)}</span>
									</div>
								</td>
							{/each}
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>
