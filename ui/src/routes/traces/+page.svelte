<script lang="ts">
	import Inbox from '@lucide/svelte/icons/inbox';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Pause from '@lucide/svelte/icons/pause';
	import Play from '@lucide/svelte/icons/play';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api, type Trace, type TraceRow } from '$lib/api/client.svelte';
	import { filterCount, filterSearch, readFilters, type TraceFilters } from '$lib/api/traces';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import FilterBar from '$lib/components/FilterBar.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PaginationBar from '$lib/components/PaginationBar.svelte';
	import PeekPanel from '$lib/components/PeekPanel.svelte';
	import TraceDetail from '$lib/components/TraceDetail.svelte';
	import TraceTable from '$lib/components/TraceTable.svelte';
	import { cost, count, duration, timestamp } from '$lib/format';
	import { DEFAULT_PAGE_SIZE, isFirstPage, pageSearch, readPage, type PageState } from '$lib/page';
	import { neighbour, peekSearch, readPeek } from '$lib/peek';

	const POLL_MS = 5000;

	// Filters, live mode and the page all live in the URL, so what somebody is
	// looking at is a link they can send (Application contract).
	const filters = $derived(readFilters(page.url.searchParams));
	const live = $derived(page.url.searchParams.get('live') === '1');
	const filtering = $derived(filterCount(filters) > 0);
	/**
	 * The filters as one string, which is what the effects below depend on.
	 * They cannot depend on `filters`: `readFilters` builds a fresh object on
	 * every URL change and a `$derived` object is never equal to the last one,
	 * so opening the panel — a `?peek=` on this same URL — re-ran the load and
	 * threw away the page the reader was on (PR #10 review).
	 */
	const filterKey = $derived(filterSearch(filters));

	const spot = $derived(readPage(page.url.searchParams));
	const newest = $derived(isFirstPage(spot));
	/** Everything that decides *which rows* this screen shows. */
	const pageKey = $derived(`${filterKey}|${spot.limit}|${spot.direction}|${spot.cursor ?? ''}`);

	let rows = $state.raw<TraceRow[]>([]);
	let nextCursor = $state.raw<string | null>(null);
	let prevCursor = $state.raw<string | null>(null);
	let total = $state.raw<{ value: number; capped: boolean } | null>(null);
	let loading = $state(true);
	let failure = $state<string | null>(null);
	// The background poll's own slot: a live tick that recovers must not erase
	// a failure the reader still needs, and a failed tick must not masquerade
	// as a failure of what is on screen.
	let liveFailure = $state<string | null>(null);

	/**
	 * Every request on this screen belongs to one page of one filter set.
	 * Changing either aborts the lot: a live tick still in flight would
	 * otherwise answer the previous query and replace the new one's rows.
	 */
	let query: AbortController | null = null;

	$effect(() => {
		// Reading the key is the subscription: this re-reads when the page or
		// the filters change and not when any other query parameter does. The
		// filter set itself is taken untracked and passed down, because `load`
		// reads it before its first `await` — inside this effect's own
		// synchronous run — and reading the object there would subscribe to it
		// after all.
		void pageKey;
		const controller = new AbortController();
		query = controller;
		load(untrack(() => filters), untrack(() => spot), controller.signal);
		return () => controller.abort();
	});

	$effect(() => {
		// The count is a question about the filters and not about the page
		// (spec 009 #4), so it is asked when they move and not on every turn.
		void filterKey;
		const controller = new AbortController();
		countMatches(untrack(() => filters), controller.signal);
		return () => controller.abort();
	});

	$effect(() => {
		// Live means "the newest page, again" (spec 009 #7): on any other page
		// there is nothing for a poll to mean.
		if (!live || !newest) return;
		// Polling, not a push channel (spec 006 #12): a hidden tab is a
		// dashboard nobody is reading, and it stops asking.
		const timer = setInterval(() => {
			if (!document.hidden) poll();
		}, POLL_MS);
		return () => clearInterval(timer);
	});

	async function load(active: TraceFilters, at: PageState, signal: AbortSignal) {
		loading = true;
		failure = null;
		liveFailure = null;
		try {
			const answer = await api.listTraces(
				active,
				{ limit: at.limit, cursor: at.cursor ?? undefined, direction: at.direction },
				signal
			);
			rows = answer.traces;
			nextCursor = answer.next_cursor;
			prevCursor = answer.prev_cursor;
			settle();
		} catch (cause) {
			if (signal.aborted) return;
			rows = [];
			nextCursor = prevCursor = null;
			// A page that never arrived cannot be landed on: leaving the
			// intent set would have the *next* successful load open the panel
			// on an unrelated row (PR #11 review).
			rolling = null;
			failure = describe(cause);
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	async function countMatches(active: TraceFilters, signal: AbortSignal) {
		try {
			// One row, because the answer wanted is the count beside it.
			const answer = await api.listTraces(active, { limit: 1, count: true }, signal);
			if (signal.aborted) return;
			total =
				answer.total === undefined
					? null
					: { value: answer.total, capped: answer.total_capped ?? false };
		} catch {
			// A count nobody can produce is a number the bar leaves out, not
			// an error over a listing that arrived perfectly well.
			if (!signal.aborted) total = null;
		}
	}

	/** One live tick: the newest page again, which on this page is this page. */
	async function poll() {
		const controller = query;
		if (!controller) return;
		const { signal } = controller;
		try {
			// Counted on the way past: live streams new traces in, and a total
			// taken when the filters last moved would sit there going stale
			// for the life of the URL (PR #11 review). The count is capped, so
			// asking for it here costs nothing a poll was not already paying.
			const answer = await api.listTraces(filters, { limit: spot.limit, count: true }, signal);
			if (signal.aborted) return;
			// Replaced rather than merged: with a window anchored at "newest",
			// the page just fetched *is* the window, and merging would grow it
			// past the size the reader asked for (spec 009 #9).
			rows = answer.traces;
			nextCursor = answer.next_cursor;
			if (answer.total !== undefined) {
				total = { value: answer.total, capped: answer.total_capped ?? false };
			}
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

	/** A filter change is a new listing, so it starts at the newest page —
	 * carrying the page *size*, which is a preference and not a position. */
	function navigate(next: TraceFilters, nextLive = live) {
		const extra: Record<string, string> = {};
		if (nextLive) extra.live = '1';
		if (spot.limit !== DEFAULT_PAGE_SIZE) extra.limit = String(spot.limit);
		goto(`/traces${filterSearch(next, extra)}`, { keepFocus: true });
	}

	/** Turning a page keeps everything else in the URL, the panel included. */
	function turn(to: Partial<PageState>) {
		const search = pageSearch(page.url.searchParams, { limit: spot.limit, ...to });
		goto(`${page.url.pathname}${search}`, { keepFocus: true, noScroll: true });
	}

	// The peek panel (spec 008): the trace a row opened, beside the listing
	// that opened it.
	const peekID = $derived(readPeek(page.url.searchParams).peek);
	const selectedObs = $derived(page.url.searchParams.get('obs'));
	let peeked = $state.raw<Trace | null>(null);

	const ids = $derived(rows.map((row) => row.id));
	const previous = $derived(neighbour(ids, peekID, -1));
	const following = $derived(neighbour(ids, peekID, 1));

	/**
	 * Where to land after the page turns under a walk: `j` on the last row of
	 * a page turns it and opens the first row of the next one, so a scan does
	 * not stop at a boundary that is an artefact of paging (spec 009 #6).
	 */
	let rolling = $state.raw<'first' | 'last' | null>(null);

	function settle() {
		if (!rolling || rows.length === 0) {
			rolling = null;
			return;
		}
		const row = rolling === 'first' ? rows[0] : rows[rows.length - 1];
		rolling = null;
		peek(row.id);
	}

	function walk(step: 1 | -1) {
		const id = neighbour(ids, peekID, step);
		if (id) {
			peek(id);
			return;
		}
		if (step === 1 && nextCursor) {
			rolling = 'first';
			turn({ cursor: nextCursor });
		} else if (step === -1 && prevCursor) {
			rolling = 'last';
			turn({ cursor: prevCursor, direction: 'prev' });
		}
	}

	/** Opening pushes one entry; moving between rows replaces it (spec 008 #6). */
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
<svelte:document onvisibilitychange={() => live && newest && !document.hidden && poll()} />

<PageHeader title="Traces">
	{#snippet meta()}
		{#if loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else if total}
			<span class="tabular-nums">{count(total.value)}{total.capped ? '+' : ''}</span>
		{:else}
			<!-- The count has not landed, or could not be taken: what is on
			     screen is still a number, and an empty slot is not (PR #11). -->
			<span class="tabular-nums">{count(rows.length)}</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<!-- Not disabled when paused: `live=1` survives a page turn, and
		     disabling the toggle would be disabling the only control that can
		     unset it (PR #11 review). It says paused and still switches off. -->
		<Button
			variant={live && newest ? 'primary' : 'default'}
			onclick={() => navigate(filters, !live)}
			aria-pressed={live}
			title={live && !newest
				? 'Paused: live follows the newest page, and this is not it. Click to switch it off'
				: `Re-read the newest page every ${POLL_MS / 1000} seconds`}
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

{#if rows.length > 0 || !newest}
	<!-- The bar stays on an empty page that is not the first one: a cursor
	     whose rows are gone — swept by retention, say — would otherwise leave
	     no way back to the listing but editing the URL (PR #11 review). -->
	<TraceTable {rows} onopen={peek} selectedID={peekID} />
	<PaginationBar
		limit={spot.limit}
		rows={rows.length}
		{total}
		hasPrev={prevCursor !== null}
		hasNext={nextCursor !== null}
		onresize={(limit) => turn({ limit })}
		onfirst={() => turn({})}
		onprev={() => turn({ cursor: prevCursor ?? undefined, direction: 'prev' })}
		onnext={() => turn({ cursor: nextCursor ?? undefined })}
		onlast={() => turn({ direction: 'prev' })}
		noun="trace"
	/>
	{#if rows.length === 0 && !loading}
		<p class="text-subtle flex flex-1 items-start justify-center p-8 text-center">
			Nothing on this page any more. Use « to go back to the newest.
		</p>
	{/if}
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
		onprev={() => walk(-1)}
		onnext={() => walk(1)}
		hasPrev={previous !== null || prevCursor !== null}
		hasNext={following !== null || nextCursor !== null}
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
