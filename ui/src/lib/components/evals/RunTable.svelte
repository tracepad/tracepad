<script lang="ts">
	import GitCompareArrows from '@lucide/svelte/icons/git-compare-arrows';
	import type { Run } from '$lib/api/client.svelte';
	import { compareChoice, compareHref, short } from '$lib/evals';
	import { ABSENT, timestamp } from '$lib/format';
	import Button from '../Button.svelte';
	import StatusChip from './StatusChip.svelte';

	// A page of runs, mapping 1:1 onto what the two run listings return
	// (spec 016, Application contract): a row is a link to the run's page, and
	// the checkboxes are the second way into a comparison (#11) — two runs of
	// one dataset ticked, and Compare goes live with the pair.
	//
	// No coverage column: the listing's rows carry no summary (spec 014 — a
	// page of runs is for choosing one), and computing one here would be N
	// requests for one table (spec 016 #16). The version the run pinned stands
	// in its place.

	let {
		rows,
		withDataset = false,
		now = Date.now()
	}: {
		rows: Run[];
		/** On the project-wide listing, where the rows differ in it. */
		withDataset?: boolean;
		/** For the ages beside `running`. */
		now?: number;
	} = $props();

	// The ticked runs, in the order they were ticked: the first is `a`. Kept as
	// ids and resolved against the rows on screen, because a page turn or a
	// filter change leaves a tick behind a listing the reader is no longer
	// looking at — and Compare going live for a row nobody can see would be a
	// comparison of two runs somebody cannot check (#11).
	let ticked = $state.raw<string[]>([]);
	const chosen = $derived(
		ticked
			.map((id) => rows.find((row) => row.id === id))
			.filter((run): run is Run => run !== undefined)
	);
	const choice = $derived(compareChoice(chosen));

	function tick(run: Run, on: boolean) {
		ticked = on ? [...ticked, run.id] : ticked.filter((id) => id !== run.id);
	}

	const cell = 'truncate px-3 py-1.5';
</script>

<div class="border-border flex shrink-0 items-center gap-2 border-b px-4 py-1.5 text-sm">
	<!-- A link once it leads somewhere, a disabled button until then: the
	     reason is the title either way, so the mistake stays a tooltip rather
	     than becoming the server's 400 page (#11). -->
	{#if choice.enabled}
		<a
			href={compareHref(chosen[0].id, chosen[1].id)}
			title={choice.reason}
			class="border-accent bg-accent text-on-accent pointer-coarse:h-11 pointer-coarse:px-4 inline-flex
				h-7 items-center gap-1.5 rounded-md border px-2.5 text-sm font-medium whitespace-nowrap
				transition-colors duration-100 hover:opacity-90"
		>
			<GitCompareArrows class="size-4" />
			Compare
		</a>
	{:else}
		<Button disabled title={choice.reason}>
			<GitCompareArrows class="size-4" />
			Compare
		</Button>
	{/if}
	<span class="text-subtle text-xs">{choice.reason}</span>
</div>

<!-- The table scrolls inside its own box; the page never scrolls sideways
     (spec 006 #15). -->
<div class="min-h-0 flex-1 overflow-auto">
	<table class="w-full min-w-2xl border-collapse text-left">
		<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
			<tr class="border-border border-b">
				<th scope="col" class="w-8 px-3 py-2"><span class="sr-only">Compare</span></th>
				<th scope="col" class="w-44 px-3 py-2 font-medium">Created</th>
				{#if withDataset}
					<th scope="col" class="w-44 px-3 py-2 font-medium">Dataset</th>
				{/if}
				<th scope="col" class="px-3 py-2 font-medium">Name</th>
				<th scope="col" class="w-20 px-3 py-2 text-right font-medium">Version</th>
				<th scope="col" class="w-40 px-3 py-2 font-medium">Status</th>
				<th scope="col" class="w-44 px-3 py-2 font-medium">Finished</th>
			</tr>
		</thead>
		<tbody>
			{#each rows as row (row.id)}
				{@const lit = chosen.some((each) => each.id === row.id)}
				<tr
					class={[
						'border-border hover:bg-raised border-b transition-colors duration-100',
						lit && 'bg-accent-soft'
					]}
				>
					<td class="px-3 py-1.5">
						<input
							type="checkbox"
							checked={lit}
							onchange={(event) => tick(row, event.currentTarget.checked)}
							aria-label="Tick run {short(row.id)} to compare"
							class="accent-accent size-4 cursor-pointer align-middle"
						/>
					</td>
					<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
						<!-- Exactly one thing in the row is tabbable, and it is a real
						     link to the run's page: a run has no peek, its page is
						     where its summary lives (spec 016, Application contract). -->
						<a href="/runs/{row.id}" title={row.id} class="hover:text-fg">
							{timestamp(row.created_at)}
						</a>
					</td>
					{#if withDataset}
						<td class="text-muted {cell}">
							<a href="/datasets/{encodeURIComponent(row.dataset)}" class="hover:text-fg">
								{row.dataset}
							</a>
						</td>
					{/if}
					<td class={cell}>
						<a href="/runs/{row.id}" class="hover:text-accent">{row.name ?? short(row.id)}</a>
					</td>
					<td class="text-muted px-3 py-1.5 text-right tabular-nums">v{row.dataset_version}</td>
					<td class="px-3 py-1.5">
						<StatusChip status={row.status} since={row.created_at} {now} />
					</td>
					<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
						{row.finished_at ? timestamp(row.finished_at) : ABSENT}
					</td>
				</tr>
			{/each}
		</tbody>
	</table>
</div>
