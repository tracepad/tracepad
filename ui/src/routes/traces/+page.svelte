<script lang="ts">
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import Inbox from '@lucide/svelte/icons/inbox';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Pause from '@lucide/svelte/icons/pause';
	import Play from '@lucide/svelte/icons/play';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api, type Trace, type TraceRow } from '$lib/api/client.svelte';
	import {
		filterCount,
		filterSearch,
		mergeRows,
		readFilters,
		type TraceFilters
	} from '$lib/api/traces';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import FilterBar from '$lib/components/FilterBar.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PeekPanel from '$lib/components/PeekPanel.svelte';
	import TraceDetail from '$lib/components/TraceDetail.svelte';
	import TraceTable from '$lib/components/TraceTable.svelte';
	import { cost, count, duration, timestamp } from '$lib/format';
	import { neighbour, peekSearch, readPeek } from '$lib/peek';

	const PAGE_SIZE = 50;
	const POLL_MS = 5000;

	// Filters and live mode live in the URL, so what somebody is looking at is
	// a link they can send (Application contract).
	const filters = $derived(readFilters(page.url.searchParams));
	const live = $derived(page.url.searchParams.get('live') === '1');
	const filtering = $derived(filterCount(filters) > 0);
	/**
	 * The filters as one string, which is what the load effect below depends
	 * on. It cannot depend on `filters`: `readFilters` builds a fresh object
	 * on every URL change and a `$derived` object is never equal to the last
	 * one, so opening the panel — a `?peek=` on this same URL — re-ran the
	 * load and threw away every page the reader had already paid for, which
	 * is the one thing spec 008 #1 exists to prevent (PR #10 review).
	 */
	const filterKey = $derived(filterSearch(filters));

	let rows = $state.raw<TraceRow[]>([]);
	let cursor = $state.raw<string | null>(null);
	let loading = $state(true);
	let loadingMore = $state(false);
	let failure = $state<string | null>(null);
	// The background poll's own slot: a live tick that recovers must not erase
	// a "load more failed" the reader still needs, and a failed tick must not
	// masquerade as a failure of what is on screen.
	let liveFailure = $state<string | null>(null);

	/**
	 * Every request on this screen belongs to one set of filters. Changing
	 * them aborts the lot: a "load more" or a live tick still in flight would
	 * otherwise answer the previous query and be merged into the new one —
	 * appending rows that do not match, and continuing from the wrong cursor.
	 */
	let query: AbortController | null = null;

	$effect(() => {
		// Reading the key is the subscription: this re-reads when the filters
		// change and not when any other query parameter does. The filter set
		// itself is taken untracked and passed down, because `load` reads it
		// before its first `await` — inside this effect's own synchronous
		// run — and reading the object there would subscribe to it after all.
		void filterKey;
		const controller = new AbortController();
		query = controller;
		load(untrack(() => filters), controller.signal);
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

	async function load(active: TraceFilters, signal: AbortSignal) {
		loading = true;
		loadingMore = false;
		failure = null;
		liveFailure = null;
		try {
			const answer = await api.listTraces(active, { limit: PAGE_SIZE }, signal);
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
		const controller = query;
		if (!cursor || loadingMore || !controller) return;
		const { signal } = controller;
		loadingMore = true;
		failure = null;
		try {
			const answer = await api.listTraces(filters, { limit: PAGE_SIZE, cursor }, signal);
			// The filters moved while this was in flight: `load` has already
			// replaced the rows, and appending this page would splice the
			// previous query's traces into the new one.
			if (signal.aborted) return;
			// Merged rather than appended, for the same reason the live poll
			// merges: with live mode on, a tick may already have pulled some
			// of these rows in — a new arrival shifts the whole cursor window
			// down by one — and a keyed each-block throws on a repeated id.
			// The merge sorts on the listing's own key, so for a page that is
			// wholly older this is exactly the append it replaces.
			rows = mergeRows(rows, answer.traces);
			cursor = answer.next_cursor;
		} catch (cause) {
			if (signal.aborted) return;
			failure = describe(cause);
		} finally {
			if (!signal.aborted) loadingMore = false;
		}
	}

	/** One live tick: the first page again, folded into what is on screen. */
	async function poll() {
		const controller = query;
		if (!controller) return;
		const { signal } = controller;
		try {
			const answer = await api.listTraces(filters, { limit: PAGE_SIZE }, signal);
			if (signal.aborted) return;
			rows = mergeRows(rows, answer.traces);
			liveFailure = null;
		} catch (cause) {
			if (signal.aborted) return;
			// A server that went away mid-poll is worth saying once, but not
			// worth throwing away the rows already on screen.
			liveFailure = describe(cause);
		}
	}

	function describe(cause: unknown): string {
		return cause instanceof ApiError ? cause.message : 'Failed to read the traces.';
	}

	function navigate(next: TraceFilters, nextLive = live) {
		goto(`/traces${filterSearch(next, nextLive ? { live: '1' } : {})}`, { keepFocus: true });
	}

	// The peek panel (spec 008): the trace a row opened, beside the listing
	// that opened it. It is URL state like the filters are, so a reload comes
	// back to it and Back closes it.
	const peekID = $derived(readPeek(page.url.searchParams).peek);
	const selectedObs = $derived(page.url.searchParams.get('obs'));
	let peeked = $state.raw<Trace | null>(null);

	// Only the rows already loaded (spec 008 #7); "next" cannot mean a cursor
	// page nobody has fetched.
	const ids = $derived(rows.map((row) => row.id));
	const previous = $derived(neighbour(ids, peekID, -1));
	const following = $derived(neighbour(ids, peekID, 1));

	/** Opening pushes one entry; moving between rows replaces it (#6). */
	function peek(id: string | null) {
		const search = peekSearch(page.url.searchParams, { peek: id });
		goto(`${page.url.pathname}${search}`, {
			replaceState: id === null || peekID !== null,
			keepFocus: true,
			noScroll: true
		});
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

{#if failure ?? liveFailure}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{failure ?? liveFailure}
	</p>
{/if}

{#if rows.length > 0}
	<TraceTable {rows} onopen={peek} selectedID={peekID} />
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

{#if peekID}
	<PeekPanel
		label="Trace"
		onclose={() => peek(null)}
		onprev={() => peek(previous)}
		onnext={() => peek(following)}
		hasPrev={previous !== null}
		hasNext={following !== null}
		fullHref="/traces/{encodeURIComponent(peekID)}{selectedObs
			? `?obs=${encodeURIComponent(selectedObs)}`
			: ''}"
		fullLabel="Open this trace as a page"
	>
		{#snippet title()}
			<h2 class="truncate text-lg font-semibold tracking-tight">
				{peeked?.name ?? 'Trace'}
			</h2>
		{/snippet}
		{#snippet meta()}
			{#if peeked}
				<span class="hidden font-mono sm:inline">{timestamp(peeked.timestamp)}</span>
				<span class="hidden tabular-nums md:inline">{duration(peeked.latency_ms)}</span>
				<span class="hidden tabular-nums md:inline">{cost(peeked.total_cost)}</span>
				<span class="hidden truncate font-mono lg:inline">{peeked.id}</span>
				<CopyButton text={peeked.id} label="Copy the trace id" />
			{/if}
		{/snippet}
		<TraceDetail traceID={peekID} bind:trace={peeked} />
	</PeekPanel>
{/if}
