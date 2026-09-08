<script lang="ts">
	import type { ScoreBreakdownRow } from '$lib/api/quality';
	import { count } from '$lib/format';

	// The categorical half of the Quality detail view (spec 025 #11): a table
	// with the same proportion bar `BreakdownTable` draws, over the two columns
	// a score has — how many were graded, and what they said.
	//
	// Written fresh rather than by generalising `BreakdownTable` (Decision 13,
	// Decision 16): that component's three columns are traces, errors and cost,
	// and a score has neither an error count nor a cost. Making it take a column
	// spec would have been a larger change to five call sites on two shipped
	// screens than these forty lines are.

	let {
		title,
		label,
		summaryLabel,
		rows,
		note
	}: {
		title: string;
		/** What the first column holds, e.g. "Model". */
		label: string;
		/** What the third column holds: "Mean", "Rate" or "Categories". */
		summaryLabel: string;
		rows: ScoreBreakdownRow[];
		/**
		 * What this breakdown does *not* count, when that is worth saying.
		 * The model one is: a score written on the trace has no model, so an
		 * empty table there is an answer rather than a gap (spec 025 #6).
		 */
		note?: string;
	} = $props();
</script>

<section class="border-border bg-surface min-w-0 rounded-lg border">
	<h2 class="text-subtle border-border border-b px-3 py-2 text-xs font-medium">{title}</h2>
	{#if rows.length === 0}
		<p class="text-subtle px-3 pt-6 pb-2 text-center text-sm">Nothing in this window</p>
	{:else}
		<div class="overflow-x-auto">
			<table class="w-full min-w-md border-collapse text-left">
				<thead class="text-subtle text-xs whitespace-nowrap">
					<tr class="border-border border-b">
						<th scope="col" class="px-3 py-1.5 font-medium">{label}</th>
						<th scope="col" class="w-32 px-3 py-1.5 font-medium">Scores</th>
						<th scope="col" class="w-48 px-3 py-1.5 font-medium">{summaryLabel}</th>
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
							<td class="px-3 py-1.5">
								<!-- The bar sits behind the number rather than beside it,
								     so a long column does not push the figures out of
								     alignment. -->
								<div class="relative">
									<div
										class="bg-accent absolute inset-y-0 left-0 rounded-sm opacity-20"
										style:width="{Math.round(row.countShare * 100)}%"
									></div>
									<span class="relative tabular-nums">{count(row.count)}</span>
								</div>
							</td>
							<td class="px-3 py-1.5 tabular-nums">{row.summary}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
	{#if note}
		<p class="text-subtle border-border border-t px-3 py-1.5 text-xs">{note}</p>
	{/if}
</section>
