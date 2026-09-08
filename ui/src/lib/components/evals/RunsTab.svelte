<script lang="ts">
	import FlaskConical from '@lucide/svelte/icons/flask-conical';
	import { page } from '$app/state';
	import { api, type Dataset, type Run } from '$lib/api/client.svelte';
	import { asPage, Listing, UrlSpot } from '$lib/listing.svelte';
	import CopyButton from '../CopyButton.svelte';
	import ListingShell from '../ListingShell.svelte';
	import RunTable from './RunTable.svelte';

	// The runs tab of a dataset (spec 016 #4): the dataset's own runs over
	// `GET /api/v1/datasets/{name}/runs`, newest first, with the checkboxes
	// that are the second way into a comparison (#11).

	let { dataset }: { dataset: Dataset } = $props();

	const listing = new Listing<Run>({
		key: () => `${dataset.name}|runs`,
		spot: new UrlSpot(),
		count: false,
		read: async (at, counting, signal) => {
			const answer = await api.listDatasetRuns(dataset.name, asPage(at, counting), signal);
			return { ...answer, rows: answer.runs };
		},
		failed: 'Failed to read the runs.'
	});

	// Runs are opened by the harness, never here (#9): what the empty tab
	// offers is the call that opens one.
	const open = $derived(
		`curl -H "Authorization: Bearer <your project key>" ${page.url.origin}/api/v1/datasets/${dataset.name}/runs \\\n` +
			`  -d '{"name": "prompt v8"}'`
	);
</script>

<!-- The total is exact and already on screen: `run_count` is what the dataset
     answers with, so this listing asks for no count of its own (spec 010,
     divergence 6). -->
<ListingShell {listing} noun="run" total={{ value: dataset.run_count, capped: false }}>
	{#snippet table()}
		<RunTable rows={listing.rows} />
	{/snippet}
	{#snippet empty()}
		<div class="flex flex-1 items-start justify-center overflow-auto p-8">
			<div class="max-w-lg">
				<h2 class="flex items-center gap-2 font-medium">
					<FlaskConical class="text-subtle size-4" />
					No runs yet
				</h2>
				<p class="text-muted mt-1">
					A run is opened by the harness that will stamp its traces, and the answer carries the
					dataset version it pinned:
				</p>
				<div class="border-border bg-surface mt-3 flex items-start gap-2 rounded-md border p-3">
					<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{open}</pre>
					<CopyButton text={() => open} label="Copy the request" />
				</div>
			</div>
		</div>
	{/snippet}
</ListingShell>
