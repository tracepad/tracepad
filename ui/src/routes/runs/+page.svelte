<script lang="ts">
	import FlaskConical from '@lucide/svelte/icons/flask-conical';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type Run } from '$lib/api/client.svelte';
	import { RUN_STATUSES, readRunFilters, runSearch, type RunFilters } from '$lib/api/runs';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import RunTable from '$lib/components/evals/RunTable.svelte';
	import ListingCount from '$lib/components/ListingCount.svelte';
	import ListingShell from '$lib/components/ListingShell.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import { asPage, Listing, UrlSpot } from '$lib/listing.svelte';
	import { freshSearch } from '$lib/page';

	// Every dataset's runs over `GET /api/v1/runs` (spec 016 #2): "what ran
	// lately, whatever the set". The two filters the endpoint takes are the
	// bar; the count is the capped one every screen-facing listing offers.

	const filters = $derived(readRunFilters(page.url.searchParams));
	const filtering = $derived(Object.keys(filters).length > 0);

	const listing = new Listing<Run>({
		key: () => runSearch(filters),
		spot: new UrlSpot(),
		read: async (at, counting, signal) => {
			const answer = await api.listRuns(filters, asPage(at, counting), signal);
			return { ...answer, rows: answer.runs };
		},
		failed: 'Failed to read the runs.'
	});

	/** A filter change is a new listing, so it starts at the newest page. */
	function navigate(next: RunFilters) {
		goto(`/runs${freshSearch(runSearch(next), page.url.searchParams)}`, { keepFocus: true });
	}

	// The dataset select is fed by the datasets listing, once: a select of
	// names is the one place the page needs them, and five hundred is more
	// datasets than a project names.
	let datasets = $state.raw<string[]>([]);
	$effect(() => {
		const controller = new AbortController();
		api
			.listDatasets({ limit: 500 }, controller.signal)
			.then((answer) => {
				if (!controller.signal.aborted) datasets = answer.datasets.map((row) => row.name);
			})
			.catch(() => {
				// The select then offers what the URL names and nothing else;
				// the listing beside it says what went wrong.
			});
		return () => controller.abort();
	});
	// A dataset the URL names that the page has not seen still has to be
	// shown, or the control would lie about the state it is in.
	const options = $derived(
		filters.dataset && !datasets.includes(filters.dataset)
			? [filters.dataset, ...datasets]
			: datasets
	);

	function pick(name: 'dataset' | 'status', value: string) {
		const next = { ...filters };
		if (value) (next as Record<string, string>)[name] = value;
		else delete next[name];
		navigate(next);
	}

	const open = $derived(
		`curl -H "Authorization: Bearer <your project key>" ${page.url.origin}/api/v1/datasets/<dataset>/runs \\\n` +
			`  -d '{"name": "prompt v8"}'`
	);

	const fieldClass =
		'border-border bg-canvas text-fg rounded-md border px-2 py-1 text-sm';
</script>

<svelte:head><title>Runs · Tracepad</title></svelte:head>

<PageHeader title="Runs">
	{#snippet meta()}
		<ListingCount {listing} />
	{/snippet}
	{#snippet actions()}
		<Button onclick={() => listing.reload()} busy={listing.loading} title="Read the listing again">
			<RefreshCw class="size-4" />
			Refresh
		</Button>
	{/snippet}
</PageHeader>

<div class="border-border overflow-x-auto border-b px-4 py-2">
	<div class="flex min-w-0 items-center gap-1.5">
		<label class="sr-only" for="run-dataset">Dataset</label>
		<select
			id="run-dataset"
			value={filters.dataset ?? ''}
			onchange={(event) => pick('dataset', event.currentTarget.value)}
			class={fieldClass}
		>
			<option value="">Any dataset</option>
			{#each options as name (name)}
				<option value={name}>{name}</option>
			{/each}
		</select>
		<label class="sr-only" for="run-status">Status</label>
		<select
			id="run-status"
			value={filters.status ?? ''}
			onchange={(event) => pick('status', event.currentTarget.value)}
			class={fieldClass}
		>
			<option value="">Any status</option>
			{#each RUN_STATUSES as status (status)}
				<option value={status}>{status}</option>
			{/each}
		</select>
	</div>
</div>

<ListingShell {listing} noun="run">
	{#snippet table()}
		<RunTable rows={listing.rows} withDataset />
	{/snippet}
	{#snippet empty()}
		<div class="flex flex-1 items-start justify-center overflow-auto p-8">
			<div class="max-w-lg">
				{#if filtering}
					<h2 class="font-medium">No run matches these filters</h2>
					<p class="text-muted mt-1">The filters are in the URL, so this is a link you can share — or clear.</p>
					<Button class="mt-3" onclick={() => navigate({})}>Clear filters</Button>
				{:else}
					<h2 class="flex items-center gap-2 font-medium">
						<FlaskConical class="text-subtle size-4" />
						No runs yet
					</h2>
					<!-- Runs are not created here (#9): a run opened by hand would sit
					     `running` for ever, with nothing to fill it. -->
					<p class="text-muted mt-1">
						A run is a container your harness opens around one pass over a dataset, then fills by
						stamping <code class="font-mono">tracepad.run_id</code> and
						<code class="font-mono">tracepad.item_id</code> on the traces it exports:
					</p>
					<div class="border-border bg-surface mt-3 flex items-start gap-2 rounded-md border p-3">
						<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{open}</pre>
						<CopyButton text={() => open} label="Copy the request" />
					</div>
					<a
						class="text-accent mt-3 inline-block underline underline-offset-2"
						href="https://github.com/tracepad/tracepad/blob/main/docs/datasets.md"
						target="_blank"
						rel="noreferrer"
					>
						Datasets and runs
					</a>
				{/if}
			</div>
		</div>
	{/snippet}
</ListingShell>
