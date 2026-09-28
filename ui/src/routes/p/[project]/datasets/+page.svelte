<script lang="ts">
	import Database from '@lucide/svelte/icons/database';
	import { api, type Dataset } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import Folded from '$lib/components/Folded.svelte';
	import NewDatasetDialog from '$lib/components/evals/NewDatasetDialog.svelte';
	import ListingCount from '$lib/components/ListingCount.svelte';
	import ListingShell from '$lib/components/ListingShell.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import { Fold } from '$lib/fold.svelte';
	import { count, counted, timestamp } from '$lib/format';
	import { asPage, Listing, UrlSpot } from '$lib/listing.svelte';
	import { href, project } from '$lib/project.svelte';

	// The datasets of the project over `GET /api/v1/datasets` (spec 016,
	// Application contract): the shared listing (#3) with no count, because
	// the endpoint offers none — datasets are as many as a person named.

	const listing = new Listing<Dataset & { id: string }>({
		key: () => 'datasets',
		spot: new UrlSpot(),
		count: false,
		read: async (at, counting, signal) => {
			const answer = await api.listDatasets(asPage(at, counting), signal);
			return { ...answer, rows: answer.datasets.map((row) => ({ ...row, id: row.name })) };
		},
		failed: 'Failed to read the datasets.'
	});

	// The whole loop, in six lines (#15): a person landing here with no
	// dataset is usually the one about to write the harness.
	const loop = [
		'tracepad score-configs push accuracy --file accuracy.json',
		'tracepad datasets push support-golden --file cases.jsonl',
		'RUN=$(tracepad runs create support-golden --name "prompt v8" --json)',
		'tracepad datasets show support-golden --version "$(echo "$RUN" | jq -r .dataset_version)" --json > cases.json',
		'# run each case under a trace stamped tracepad.run_id / tracepad.item_id, post the scores',
		'tracepad runs finish "$(echo "$RUN" | jq -r .id)"'
	].join('\n');

	let creating = $state(false);

	const cell = 'truncate px-3 py-1.5';
	const numeric = 'px-3 py-1.5 text-right tabular-nums';

	// In a box narrower than the table a row is the dataset's name, and what
	// the other columns say folds under it (spec 006 #22). The number is the
	// unfolded table's width and its `min-width`.
	const fold = new Fold(880);
	const narrow = $derived(fold.narrow);
</script>

<svelte:head><title>Datasets · Tracepad</title></svelte:head>

<PageHeader title="Datasets">
	{#snippet meta()}
		<ListingCount {listing} />
	{/snippet}
	{#snippet actions()}
		{#if project.editor}
			<Button variant="primary" onclick={() => (creating = true)}>New dataset</Button>
		{/if}
	{/snippet}
</PageHeader>

<NewDatasetDialog open={creating} onclose={() => (creating = false)} />

<ListingShell {listing} noun="dataset" back="first">
	{#snippet table()}
		<div bind:clientWidth={fold.box} class="min-h-0 flex-1 overflow-auto">
			<table class="w-full border-collapse text-left" style:min-width={fold.min}>
				<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
					<tr class="border-border border-b">
						<th scope="col" class={['px-3 py-2 font-medium', !narrow && 'w-56']}>Name</th>
						{#if !narrow}
							<th scope="col" class="px-3 py-2 font-medium">Description</th>
							<th scope="col" class="w-20 px-3 py-2 text-right font-medium">Version</th>
							<th scope="col" class="w-20 px-3 py-2 text-right font-medium">Items</th>
							<th scope="col" class="w-20 px-3 py-2 text-right font-medium">Runs</th>
							<th scope="col" class="w-44 px-3 py-2 font-medium">Updated</th>
						{/if}
					</tr>
				</thead>
				<tbody>
					{#each listing.rows as row (row.name)}
						<tr class="border-border hover:bg-raised border-b transition-colors duration-100">
							<td class={[cell, 'font-medium', narrow && 'max-w-0']}>
								<a href={href(`/datasets/${encodeURIComponent(row.name)}`)} class="hover:text-accent">
									{row.name}
								</a>
								{#if narrow}
									{#if row.description}
										<div class="text-muted truncate text-xs font-normal">{row.description}</div>
									{/if}
									<div class="text-muted text-xs font-normal whitespace-normal tabular-nums">
										<Folded
											values={[
												`v${row.version}`,
												counted(row.item_count, 'item'),
												counted(row.run_count, 'run'),
												`updated ${timestamp(row.updated_at)}`
											]}
										/>
									</div>
								{/if}
							</td>
							{#if !narrow}
								<td class="text-muted {cell}">{row.description ?? '—'}</td>
								<td class="text-muted {numeric}">v{row.version}</td>
								<td class="text-muted {numeric}">{count(row.item_count)}</td>
								<td class="text-muted {numeric}">{count(row.run_count)}</td>
								<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
									{timestamp(row.updated_at)}
								</td>
							{/if}
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/snippet}
	{#snippet empty()}
		<div class="flex flex-1 items-start justify-center overflow-auto p-8">
			<div class="max-w-2xl">
				<h2 class="flex items-center gap-2 font-medium">
					<Database class="text-subtle size-4" />
					No datasets yet
				</h2>
				<p class="text-muted mt-1">
					A dataset is a versioned set of test cases; a run is one pass of your harness over it, with
					its traces stamped so the store can link them. Tracepad executes nothing — the whole loop is
					six lines:
				</p>
				<div class="border-border bg-surface mt-3 flex items-start gap-2 rounded-md border p-3">
					<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{loop}</pre>
					<CopyButton text={() => loop} label="Copy the loop" />
				</div>
				<a
					class="text-accent mt-3 inline-block underline underline-offset-2"
					href="https://github.com/tracepad/tracepad/blob/main/docs/datasets.md"
					target="_blank"
					rel="noreferrer"
				>
					Datasets and runs
				</a>
			</div>
		</div>
	{/snippet}
</ListingShell>
