<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import MessagesSquare from '@lucide/svelte/icons/messages-square';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type Session, type SessionRow, type Trace } from '$lib/api/client.svelte';
	import { readSessionFilters, sessionSearch, type SessionFilters } from '$lib/api/sessions';
	import PaginationBar from '$lib/components/PaginationBar.svelte';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PeekPanel from '$lib/components/PeekPanel.svelte';
	import RangePicker from '$lib/components/RangePicker.svelte';
	import SessionDetail from '$lib/components/SessionDetail.svelte';
	import SessionTable from '$lib/components/SessionTable.svelte';
	import TraceDetail from '$lib/components/TraceDetail.svelte';
	import { cost, count, duration, timestamp } from '$lib/format';
	import { asPage, Listing, UrlSpot, Walk } from '$lib/listing.svelte';
	import { freshSearch } from '$lib/page';
	import { peekSearch, readPeek } from '$lib/peek';

	// Sessions over `GET /api/v1/sessions`, the endpoint spec 007 added for all
	// three clients at once. No live mode here (spec 007 #8): a session's totals
	// move only when its traces do, and the Traces screen is where arrival is
	// watched. This one re-reads on a filter change and on the refresh control,
	// which is the whole of its update story — `reload()` rather than `tick()`,
	// because a Refresh is a page turn that lands where it started (spec 010 #5).

	const filters = $derived(readSessionFilters(page.url.searchParams));
	const filtering = $derived(Object.keys(filters).length > 0);

	const listing = new Listing<SessionRow>({
		key: () => sessionSearch(filters),
		spot: new UrlSpot(),
		read: async (at, counting, signal) => {
			const answer = await api.listSessions(filters, asPage(at, counting), signal);
			return { ...answer, rows: answer.sessions };
		},
		failed: 'Failed to read the sessions.'
	});

	/** A filter change starts at the newest page, carrying the page size. */
	function navigate(next: SessionFilters) {
		goto(`/sessions${freshSearch(sessionSearch(next), page.url.searchParams)}`, {
			keepFocus: true
		});
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

	const walk = new Walk(listing, {
		// `last_seen` is the `MAX(timestamp)` this listing is sorted by, and
		// under a filter it is not the session's own span — so a filtered
		// listing places only the rows it has seen (spec 009 #14).
		key: (row) => row.last_seen ?? '',
		peekID: () => peekID,
		showing: () =>
			!filtering && peekedSession?.last_seen
				? { id: peekedSession.id, key: peekedSession.last_seen }
				: null,
		open: (id) => peek(id)
	});

	/** Deepening pushes one history entry; moving sideways or up replaces (#6). */
	function move(next: { peek: string | null; trace?: string | null }, deeper: boolean) {
		const search = peekSearch(page.url.searchParams, next);
		goto(`${page.url.pathname}${search}`, {
			replaceState: !deeper,
			keepFocus: true,
			noScroll: true
		});
	}

	/**
	 * The trace the panel last had open, and the session it belonged to.
	 * Coming back up clears `trace` in the same render that un-hides the
	 * session's table, so `drilled` is already null when the row would be
	 * lit — this is what says "you were here" on the way back (PR #10, third
	 * review: the second round claimed this and did not do it).
	 *
	 * Recorded from the URL rather than from the click, so that it survives a
	 * reload on the trace layer; paired with its session, so that walking to
	 * another one lights nothing rather than a row that is not there.
	 */
	let visited = $state.raw<{ session: string | null; trace: string } | null>(null);
	$effect(() => {
		if (drilled) visited = { session: peekID, trace: drilled };
	});
	const lastDrilled = $derived(visited?.session === peekID ? (visited?.trace ?? null) : null);

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
		{#if listing.loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else if listing.total}
			<span class="tabular-nums">{count(listing.total.value)}{listing.total.capped ? '+' : ''}</span>
		{:else}
			<!-- The count has not landed, or could not be taken (PR #11). -->
			<span class="tabular-nums">{count(listing.rows.length)}</span>
		{/if}
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

{#if listing.problem}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{listing.problem}
	</p>
{/if}

{#if listing.rows.length > 0 || !listing.newest}
	<!-- The bar stays on an empty page that is not the first one, so a cursor
	     whose rows are gone still has a way back (PR #11 review). -->
	<SessionTable rows={listing.rows} onopen={peek} selectedID={peekID} />
	<PaginationBar {...listing.bar} noun="session" />
	{#if listing.rows.length === 0 && !listing.loading}
		<p class="text-subtle flex flex-1 items-start justify-center p-8 text-center">
			Nothing on this page any more. Use « to go back to the newest.
		</p>
	{/if}
{:else if !listing.loading && !listing.failure}
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
		onprev={drilled ? undefined : () => walk.step(-1)}
		onnext={drilled ? undefined : () => walk.step(1)}
		hasPrev={walk.hasPrev}
		hasNext={walk.hasNext}
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
					{#if peekedTrace.release}
						<span class="hidden truncate md:inline" title="Release">{peekedTrace.release}</span>
					{/if}
					<span class="hidden tabular-nums md:inline">{duration(peekedTrace.latency_ms)}</span>
					<span class="hidden tabular-nums md:inline">{cost(peekedTrace.total_cost)}</span>
				{/if}
			{:else}
				<span class="truncate font-mono">{peekID}</span>
				<CopyButton text={peekID} label="Copy the session id" />
			{/if}
		{/snippet}

		<!-- Hidden rather than unmounted while a trace is open over it: the
		     session's own listing has loaded pages and a scroll position, and
		     the breadcrumb back would pay for both again — which is the cost
		     Decisions 1 and 9 exist to avoid, one level down (PR #10, second
		     review). The classes are exclusive rather than a `hidden` added to
		     a `flex`, so that neither has to win on stylesheet order. -->
		<div class={drilled ? 'hidden' : 'flex min-h-0 flex-1 flex-col'}>
			<SessionDetail
				sessionID={peekID}
				bind:session={peekedSession}
				onopen={drill}
				selectedTraceID={lastDrilled}
			/>
		</div>
		{#if drilled}
			<TraceDetail traceID={drilled} bind:trace={peekedTrace} />
		{/if}
	</PeekPanel>
{/if}
