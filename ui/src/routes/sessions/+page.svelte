<script lang="ts">
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import MessagesSquare from '@lucide/svelte/icons/messages-square';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		ApiError,
		api,
		type Session,
		type SessionRow,
		type Trace
	} from '$lib/api/client.svelte';
	import { readSessionFilters, sessionSearch, type SessionFilters } from '$lib/api/sessions';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PeekPanel from '$lib/components/PeekPanel.svelte';
	import RangePicker from '$lib/components/RangePicker.svelte';
	import SessionDetail from '$lib/components/SessionDetail.svelte';
	import SessionTable from '$lib/components/SessionTable.svelte';
	import TraceDetail from '$lib/components/TraceDetail.svelte';
	import { cost, count, duration, timestamp } from '$lib/format';
	import { neighbour, peekSearch, readPeek } from '$lib/peek';

	// Sessions over `GET /api/v1/sessions`, the endpoint this spec added for
	// all three clients at once. No live mode here (spec 007 #8): a session's
	// totals move only when its traces do, and the Traces screen is where
	// arrival is watched. This one refetches on a filter change and on the
	// refresh control, which is the whole of its update story.

	const PAGE_SIZE = 50;

	const filters = $derived(readSessionFilters(page.url.searchParams));
	const filtering = $derived(Object.keys(filters).length > 0);
	/**
	 * What the load effect depends on. Not `filters`: a fresh object every
	 * time the URL moves would re-run the listing whenever the panel opened
	 * or drilled, discarding the pages already loaded (PR #10 review).
	 */
	const filterKey = $derived(sessionSearch(filters));

	let rows = $state.raw<SessionRow[]>([]);
	let cursor = $state.raw<string | null>(null);
	let loading = $state(true);
	let loadingMore = $state(false);
	let failure = $state<string | null>(null);
	/** Bumped by the refresh control to re-run the load effect. */
	let generation = $state(0);

	// Every request on this screen belongs to one set of filters; changing them
	// aborts the lot, so a "load more" in flight cannot append the previous
	// query's page onto the new one (the bug spec 006's review found).
	let query: AbortController | null = null;

	$effect(() => {
		// The key, not the filter object, and the object itself untracked:
		// `load` reads it inside this effect's own synchronous run, which
		// would subscribe to a value that is new on every URL change.
		void filterKey;
		generation;
		const controller = new AbortController();
		query = controller;
		load(untrack(() => filters), controller.signal);
		return () => controller.abort();
	});

	async function load(active: SessionFilters, signal: AbortSignal) {
		loading = true;
		loadingMore = false;
		failure = null;
		try {
			const answer = await api.listSessions(active, { limit: PAGE_SIZE }, signal);
			rows = answer.sessions;
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
			const answer = await api.listSessions(filters, { limit: PAGE_SIZE, cursor }, signal);
			if (signal.aborted) return;
			rows = [...rows, ...answer.sessions];
			cursor = answer.next_cursor;
		} catch (cause) {
			if (signal.aborted) return;
			failure = describe(cause);
		} finally {
			if (!signal.aborted) loadingMore = false;
		}
	}

	function describe(cause: unknown): string {
		return cause instanceof ApiError ? cause.message : 'Failed to read the sessions.';
	}

	function navigate(next: SessionFilters) {
		goto(`/sessions${sessionSearch(next)}`, { keepFocus: true });
	}

	// The peek panel (spec 008). A session opens beside the listing, and a
	// trace inside it replaces the panel's body one level deep (#9) — a
	// session is a conversation, and reading one means walking its traces
	// without losing the session.
	const opened = $derived(readPeek(page.url.searchParams));
	const peekID = $derived(opened.peek);
	const drilled = $derived(opened.trace);
	const selectedObs = $derived(page.url.searchParams.get('obs'));
	let peekedSession = $state.raw<Session | null>(null);
	let peekedTrace = $state.raw<Trace | null>(null);

	const ids = $derived(rows.map((row) => row.id));
	const previous = $derived(neighbour(ids, peekID, -1));
	const following = $derived(neighbour(ids, peekID, 1));

	/** Deepening pushes one history entry; moving sideways or up replaces (#6). */
	function move(next: { peek: string | null; trace?: string | null }, deeper: boolean) {
		const search = peekSearch(page.url.searchParams, next);
		goto(`${page.url.pathname}${search}`, {
			replaceState: !deeper,
			keepFocus: true,
			noScroll: true
		});
	}

	const peek = (id: string | null) => move({ peek: id }, id !== null && peekID === null);
	const drill = (traceID: string | null) =>
		move({ peek: peekID, trace: traceID }, traceID !== null);

	/** Commits a text filter on blur or Enter, never on every keystroke. */
	function commit(name: 'environment' | 'user_id', value: string) {
		const next = { ...filters };
		if (value.trim()) next[name] = value.trim();
		else delete next[name];
		navigate(next);
	}

	const fieldClass =
		'border-border bg-canvas placeholder:text-subtle w-40 rounded-md border px-2 py-1 text-sm';
</script>

<svelte:head><title>Sessions · Tracepad</title></svelte:head>

<PageHeader title="Sessions">
	{#snippet meta()}
		{#if loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else}
			<span class="tabular-nums">{count(rows.length)}{cursor ? '+' : ''}</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button onclick={() => generation++} busy={loading} title="Read the listing again">
			<RefreshCw class="size-4" />
			Refresh
		</Button>
	{/snippet}
</PageHeader>

<div class="border-border overflow-x-auto border-b px-4 py-2">
	<div class="flex min-w-0 items-center gap-1.5">
		<RangePicker
			range={{ from: filters.from, to: filters.to }}
			onchange={(range) => navigate({ environment: filters.environment, user_id: filters.user_id, ...range })}
		/>
		<label class="sr-only" for="session-environment">Environment</label>
		<input
			id="session-environment"
			type="text"
			value={filters.environment ?? ''}
			onchange={(event) => commit('environment', event.currentTarget.value)}
			placeholder="Environment"
			autocomplete="off"
			spellcheck="false"
			class={fieldClass}
		/>
		<label class="sr-only" for="session-user">User</label>
		<input
			id="session-user"
			type="text"
			value={filters.user_id ?? ''}
			onchange={(event) => commit('user_id', event.currentTarget.value)}
			placeholder="User id"
			autocomplete="off"
			spellcheck="false"
			class={fieldClass}
		/>
	</div>
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
	<SessionTable {rows} onopen={peek} selectedID={peekID} />
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
				<h2 class="font-medium">No session matches these filters</h2>
				<p class="text-muted mt-1">
					The filters are in the URL, so this is a link you can share — or clear.
				</p>
				<Button class="mt-3" onclick={() => navigate({})}>Clear filters</Button>
			{:else}
				<h2 class="flex items-center gap-2 font-medium">
					<MessagesSquare class="text-subtle size-4" />
					No sessions yet
				</h2>
				<p class="text-muted mt-1">
					A session is a group of traces that share a <code class="font-mono">session.id</code>. Set
					it on your traces — most SDKs take it as <code class="font-mono">session_id</code> — and
					every conversation or agent run shows up here as one row.
				</p>
			{/if}
		</div>
	</div>
{:else}
	<div class="flex-1"></div>
{/if}

{#if peekID}
	<PeekPanel
		label={drilled ? 'Trace' : 'Session'}
		onclose={() => peek(null)}
		onprev={drilled ? undefined : () => peek(previous)}
		onnext={drilled ? undefined : () => peek(following)}
		hasPrev={previous !== null}
		hasNext={following !== null}
		fullHref={drilled
			? `/traces/${encodeURIComponent(drilled)}${
					selectedObs ? `?obs=${encodeURIComponent(selectedObs)}` : ''
				}`
			: `/sessions/${encodeURIComponent(peekID)}`}
		fullLabel={drilled ? 'Open this trace as a page' : 'Open this session as a page'}
	>
		{#snippet title()}
			{#if drilled}
				<!-- The way back up: this layer replaced the session's own table,
				     so the breadcrumb is what returns to it (spec 008 #9). -->
				<button
					type="button"
					onclick={() => drill(null)}
					class="text-muted hover:text-fg pointer-coarse:min-h-11 flex shrink-0 cursor-pointer
						items-center gap-0.5 whitespace-nowrap transition-colors duration-100"
				>
					<ChevronLeft class="size-3.5" />
					Session
				</button>
				<h2 class="truncate text-lg font-semibold tracking-tight">
					{peekedTrace?.name ?? 'Trace'}
				</h2>
			{:else}
				<h2 class="shrink-0 text-lg font-semibold tracking-tight">Session</h2>
			{/if}
		{/snippet}
		{#snippet meta()}
			{#if drilled}
				{#if peekedTrace}
					<span class="hidden font-mono sm:inline">{timestamp(peekedTrace.timestamp)}</span>
					<span class="hidden tabular-nums md:inline">{duration(peekedTrace.latency_ms)}</span>
					<span class="hidden tabular-nums md:inline">{cost(peekedTrace.total_cost)}</span>
				{/if}
			{:else}
				<span class="truncate font-mono">{peekID}</span>
				<CopyButton text={peekID} label="Copy the session id" />
			{/if}
		{/snippet}

		{#if drilled}
			<TraceDetail traceID={drilled} bind:trace={peekedTrace} />
		{:else}
			<SessionDetail sessionID={peekID} bind:session={peekedSession} onopen={drill} />
		{/if}
	</PeekPanel>
{/if}
