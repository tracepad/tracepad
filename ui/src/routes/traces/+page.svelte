<script lang="ts">
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import Inbox from '@lucide/svelte/icons/inbox';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Pause from '@lucide/svelte/icons/pause';
	import Play from '@lucide/svelte/icons/play';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api, type TraceRow } from '$lib/api/client.svelte';
	import { filterSearch, mergeRows, readFilters, type TraceFilters } from '$lib/api/traces';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import FilterBar from '$lib/components/FilterBar.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import TraceTable from '$lib/components/TraceTable.svelte';
	import { count } from '$lib/format';

	const PAGE_SIZE = 50;
	const POLL_MS = 5000;

	// Filters and live mode live in the URL, so what somebody is looking at is
	// a link they can send (Application contract).
	const filters = $derived(readFilters(page.url.searchParams));
	const live = $derived(page.url.searchParams.get('live') === '1');
	const filtering = $derived(page.url.search !== '' && page.url.search !== '?live=1');

	let rows = $state.raw<TraceRow[]>([]);
	let cursor = $state.raw<string | null>(null);
	let loading = $state(true);
	let loadingMore = $state(false);
	let failure = $state<string | null>(null);

	$effect(() => {
		// Re-reads whenever the filters in the URL change. `filterSearch` is
		// what makes that a dependency, and it is also the cheapest way to
		// compare two filter sets.
		filterSearch(filters);
		const controller = new AbortController();
		load(controller.signal);
		return () => controller.abort();
	});

	$effect(() => {
		if (!live) return;
		// Polling, not a push channel (spec 006 #12): a hidden tab is a
		// dashboard nobody is reading, and it stops asking.
		const timer = setInterval(() => {
			if (!document.hidden) poll();
		}, POLL_MS);
		return () => clearInterval(timer);
	});

	async function load(signal: AbortSignal) {
		loading = true;
		failure = null;
		try {
			const answer = await api.listTraces(filters, { limit: PAGE_SIZE }, signal);
			rows = answer.traces;
			cursor = answer.next_cursor;
		} catch (cause) {
			if (signal.aborted) return;
			rows = [];
			failure = describe(cause);
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	async function loadMore() {
		if (!cursor || loadingMore) return;
		loadingMore = true;
		try {
			const answer = await api.listTraces(filters, { limit: PAGE_SIZE, cursor });
			rows = [...rows, ...answer.traces];
			cursor = answer.next_cursor;
		} catch (cause) {
			failure = describe(cause);
		} finally {
			loadingMore = false;
		}
	}

	/** One live tick: the first page again, folded into what is on screen. */
	async function poll() {
		try {
			const answer = await api.listTraces(filters, { limit: PAGE_SIZE });
			rows = mergeRows(rows, answer.traces);
			failure = null;
		} catch (cause) {
			// A server that went away mid-poll is worth saying once, but not
			// worth throwing away the rows already on screen.
			failure = describe(cause);
		}
	}

	function describe(cause: unknown): string {
		return cause instanceof ApiError ? cause.message : 'Failed to read the traces.';
	}

	function navigate(next: TraceFilters, nextLive = live) {
		goto(`/traces${filterSearch(next, nextLive ? { live: '1' } : {})}`, { keepFocus: true });
	}

	const snippet = $derived(
		`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=${page.url.origin}/v1/traces\n` +
			'OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer <your project key>"'
	);
</script>

<svelte:head><title>Traces · Tracepad</title></svelte:head>
<!-- Coming back to a tab that was paused should not wait out the interval. -->
<svelte:document onvisibilitychange={() => live && !document.hidden && poll()} />

<PageHeader title="Traces">
	{#snippet meta()}
		{#if loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else}
			<span class="tabular-nums">{count(rows.length)}{cursor ? '+' : ''}</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button
			variant={live ? 'primary' : 'default'}
			onclick={() => navigate(filters, !live)}
			aria-pressed={live}
			title="Re-read the newest page every {POLL_MS / 1000} seconds"
		>
			{#if live}<Pause class="size-4" />{:else}<Play class="size-4" />{/if}
			Live
		</Button>
	{/snippet}
</PageHeader>

<div class="border-border overflow-x-auto border-b px-4 py-2">
	<FilterBar {filters} onchange={(next) => navigate(next)} />
</div>

{#if failure}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{failure}
	</p>
{/if}

{#if rows.length > 0}
	<TraceTable {rows} />
	<div class="border-border flex shrink-0 items-center justify-center border-t px-4 py-2">
		{#if cursor}
			<Button onclick={loadMore} busy={loadingMore}>
				{#if loadingMore}
					<LoaderCircle class="size-4 animate-spin" />
				{:else}
					<ChevronDown class="size-4" />
				{/if}
				Load more
			</Button>
		{:else}
			<span class="text-subtle text-xs">End of the listing</span>
		{/if}
	</div>
{:else if !loading && !failure}
	<div class="flex flex-1 items-start justify-center overflow-auto p-8">
		<div class="max-w-lg">
			{#if filtering}
				<h2 class="font-medium">No trace matches these filters</h2>
				<p class="text-muted mt-1">
					The filters are in the URL, so this is a link you can share — or clear.
				</p>
				<Button class="mt-3" onclick={() => navigate({})}>Clear filters</Button>
			{:else}
				<h2 class="flex items-center gap-2 font-medium">
					<Inbox class="text-subtle size-4" />
					No traces yet
				</h2>
				<p class="text-muted mt-1">
					Point an OpenTelemetry-instrumented app at this server and reload.
				</p>
				<div class="border-border bg-surface mt-3 flex items-start gap-2 rounded-md border p-3">
					<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{snippet}</pre>
					<CopyButton text={() => snippet} label="Copy the exporter settings" />
				</div>
				<p class="text-subtle mt-2 text-xs">
					The key is the one printed when this project was created; nobody else, including this
					page, can read it back.
				</p>
				<a
					class="text-accent mt-3 inline-block underline underline-offset-2"
					href="https://github.com/tracepad/tracepad/blob/main/docs/quickstart.md"
					target="_blank"
					rel="noreferrer"
				>
					Quickstart
				</a>
			{/if}
		</div>
	</div>
{:else}
	<div class="flex-1"></div>
{/if}
