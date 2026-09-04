<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api, type Dataset } from '$lib/api/client.svelte';
	import ItemsTab from '$lib/components/evals/ItemsTab.svelte';
	import RunsTab from '$lib/components/evals/RunsTab.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import { count } from '$lib/format';
	import { freshSearch } from '$lib/page';

	// One dataset (spec 016 #4): its envelope in the header, a version
	// selector that rewrites the items tab to `?version=`, and two tabs in the
	// URL — the items and the runs are two listings of one thing with
	// different columns and different peeks. An older version is read-only and
	// says so: an edit *is* a new version at the head.

	const name = $derived(page.params.name ?? '');
	const tab = $derived(page.url.searchParams.get('tab') === 'runs' ? 'runs' : 'items');

	let dataset = $state.raw<Dataset | null>(null);
	let failure = $state<string | null>(null);

	$effect(() => {
		const controller = new AbortController();
		void load(name, controller.signal);
		return () => controller.abort();
	});

	async function load(wanted: string, signal: AbortSignal) {
		failure = null;
		try {
			const answer = await api.getDataset(wanted, signal);
			if (!signal.aborted) dataset = answer;
		} catch (cause) {
			if (signal.aborted) return;
			dataset = null;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the dataset.';
		}
	}

	// The version in force: the URL's when it names one the dataset has, the
	// current one otherwise. Absent from the URL at the head, so the plain
	// link is the plain dataset.
	const asked = $derived(Number(page.url.searchParams.get('version')));
	const version = $derived.by(() => {
		if (!dataset) return undefined;
		const valid = Number.isInteger(asked) && asked >= 0 && asked <= dataset.version;
		return valid && page.url.searchParams.has('version') ? asked : dataset.version;
	});
	const older = $derived(dataset !== null && version !== undefined && version < dataset.version);

	/** A tab or version change is a fresh listing: first page, same size. */
	function navigate(next: { tab: 'items' | 'runs'; version?: number }) {
		const params = new URLSearchParams();
		if (next.tab === 'runs') params.set('tab', 'runs');
		if (next.version !== undefined && dataset && next.version !== dataset.version) {
			params.set('version', String(next.version));
		}
		const search = params.toString();
		goto(`/datasets/${encodeURIComponent(name)}${freshSearch(search ? `?${search}` : '', page.url.searchParams)}`, {
			keepFocus: true
		});
	}

	function pick(event: Event) {
		const wanted = Number((event.currentTarget as HTMLInputElement).value);
		if (Number.isInteger(wanted)) navigate({ tab: 'items', version: wanted });
	}

	const tabClass = (active: boolean) =>
		[
			'border-b-2 px-3 py-2 text-sm font-medium transition-colors duration-100',
			active ? 'border-accent text-fg' : 'text-muted hover:text-fg border-transparent'
		].join(' ');
</script>

<svelte:head><title>{name} · Datasets · Tracepad</title></svelte:head>

<PageHeader title={name}>
	{#snippet meta()}
		<a href="/datasets" class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			Datasets
		</a>
		{#if dataset}
			<span class="hidden truncate md:inline" title={dataset.description ?? undefined}>
				{dataset.description ?? ''}
			</span>
			<!-- Hidden at a phone's width: the version control beside it needs
			     the room, and both numbers are one tab away. -->
			<span class="hidden tabular-nums whitespace-nowrap sm:inline">
				{count(dataset.item_count)} items · {count(dataset.run_count)} runs
			</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		{#if dataset}
			<!-- A number, not a dropdown of every version: a dataset edited in
			     CI has hundreds (edge cases), and the current is the max. -->
			<label class="text-subtle flex items-center gap-1.5 text-xs whitespace-nowrap">
				<span class="hidden sm:inline">Version</span>
				<input
					id="dataset-version"
					name="version"
					type="number"
					min="0"
					max={dataset.version}
					value={version}
					onchange={pick}
					aria-label="Dataset version"
					class="border-border bg-canvas text-fg w-16 rounded-md border px-1.5 py-1 text-sm tabular-nums"
				/>
				<span class="tabular-nums">of {dataset.version}</span>
			</label>
		{/if}
	{/snippet}
</PageHeader>

{#if failure}
	<div class="flex flex-1 items-start justify-center p-8">
		<p role="alert" class="text-danger flex max-w-md items-start gap-2">
			<TriangleAlert class="mt-0.5 size-4 shrink-0" />
			{failure}
		</p>
	</div>
{:else if !dataset}
	<div class="text-subtle flex flex-1 items-center justify-center gap-2">
		<LoaderCircle class="size-4 animate-spin" />
		Loading the dataset
	</div>
{:else}
	<nav class="border-border flex shrink-0 border-b px-2" aria-label="Dataset tabs">
		<a
			href="/datasets/{encodeURIComponent(name)}{older ? `?version=${version}` : ''}"
			aria-current={tab === 'items' ? 'true' : undefined}
			onclick={(event) => {
				event.preventDefault();
				navigate({ tab: 'items', version });
			}}
			class={tabClass(tab === 'items')}
		>
			Items
		</a>
		<a
			href="/datasets/{encodeURIComponent(name)}?tab=runs"
			aria-current={tab === 'runs' ? 'true' : undefined}
			onclick={(event) => {
				event.preventDefault();
				navigate({ tab: 'runs' });
			}}
			class={tabClass(tab === 'runs')}
		>
			Runs
		</a>
	</nav>

	{#if tab === 'items' && older}
		<p class="text-warn border-border border-b px-4 py-2 text-sm">
			Reading version {version} of {dataset.version}. An older version is read-only: an edit is
			a new version at the head.
		</p>
	{/if}

	{#if tab === 'items'}
		<ItemsTab {dataset} version={version ?? dataset.version} />
	{:else}
		<RunsTab {dataset} />
	{/if}
{/if}
