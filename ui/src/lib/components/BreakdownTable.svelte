<script lang="ts">
	import type { BreakdownRow } from '$lib/api/stats';
	import { Fold } from '$lib/fold.svelte';
	import { cost, count, counted } from '$lib/format';
	import Folded from './Folded.svelte';

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

	// In a box narrower than the table the row is its key, how many and how
	// many failed, each with its bar; the cost and the tokens fold under the
	// key (spec 006 #24). The number is the unfolded table's width and its
	// `min-width`, a column narrower without the tokens.
	const fold = new Fold(() => (tokens ? 480 : 400));
	const narrow = $derived(fold.narrow);
	const shown = $derived(narrow ? cells.slice(0, 2) : cells);
</script>

<section class="border-border bg-surface min-w-0 rounded-lg border">
	<h2 class="text-subtle border-border border-b px-3 py-2 text-xs font-medium">{title}</h2>
	{#if rows.length === 0}
		<p class="text-subtle px-3 py-6 text-center text-sm">Nothing in this window</p>
	{:else}
		<div bind:contentRect={fold.rect} class="overflow-x-auto">
			<table class="w-full border-collapse text-left" style:min-width={fold.min}>
				<thead class="text-subtle text-xs whitespace-nowrap">
					<tr class="border-border border-b">
						<th scope="col" class={['px-3 py-1.5 font-medium', narrow ? 'w-full' : 'w-64']}>{label}</th>
						<th scope="col" class={['px-3 py-1.5 font-medium', !narrow && 'w-44']}>
							{unit === 'observation' ? 'Observations' : 'Traces'}
						</th>
						<th scope="col" class={['px-3 py-1.5 font-medium', !narrow && 'w-36']}>Errors</th>
						{#if !narrow}
							<th scope="col" class="w-36 px-3 py-1.5 font-medium">Cost</th>
						{/if}
						{#if tokens && !narrow}
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
							{#if narrow}
								<!-- A cell, not the row's header, for the reason the Accounts
								     card gives (spec 006 #18): it holds the cost and the tokens
								     too, and a header is read before every cell of the row. -->
								<td class="max-w-0 px-3 py-1.5 text-xs">
									<div class="truncate font-mono" title={row.key}>{row.key}</div>
									<div class="text-muted tabular-nums">
										<Folded values={[cost(row.cost), tokens ? counted(row.tokens, 'token') : null]} />
									</div>
								</td>
							{:else}
								<th
									scope="row"
									class="max-w-0 min-w-32 truncate px-3 py-1.5 text-left font-mono text-xs font-normal"
									title={row.key}
								>
									{row.key}
								</th>
							{/if}
							{#each shown as cell, index (index)}
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
