<script lang="ts">
	import Inbox from '@lucide/svelte/icons/inbox';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Pause from '@lucide/svelte/icons/pause';
	import Play from '@lucide/svelte/icons/play';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type Trace, type TraceRow } from '$lib/api/client.svelte';
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
	import { asPage, Listing, UrlSpot, Walk } from '$lib/listing.svelte';
	import { freshSearch } from '$lib/page';
	import { peekSearch, readPeek } from '$lib/peek';

	const POLL_MS = 5000;

	// Filters, live mode and the page all live in the URL, so what somebody is
	// looking at is a link they can send (Application contract). The listing
	// itself — rows, cursors, count, aborts — is `$lib/listing` (spec 010).
	const filters = $derived(readFilters(page.url.searchParams));
	const live = $derived(page.url.searchParams.get('live') === '1');
	const filtering = $derived(filterCount(filters) > 0);

	const listing = new Listing<TraceRow>({
		key: () => filterSearch(filters),
		spot: new UrlSpot(),
		read: async (at, counting, signal) => {
			const answer = await api.listTraces(filters, asPage(at, counting), signal);
			return { ...answer, rows: answer.traces };
		},
		failed: 'Failed to read the traces.'
	});

	$effect(() => {
		// Live means "the newest page, again" (spec 009 #7): on any other page
		// there is nothing for a tick to mean.
		if (!live || !listing.newest) return;
		// Polling, not a push channel (spec 006 #12): a hidden tab is a
		// dashboard nobody is reading, and it stops asking.
		const timer = setInterval(() => {
			if (!document.hidden) listing.tick();
		}, POLL_MS);
		return () => clearInterval(timer);
	});

	/** A filter change is a new listing, so it starts at the newest page. */
	function navigate(next: TraceFilters, nextLive = live) {
		const extra: Record<string, string> = nextLive ? { live: '1' } : {};
		goto(`/traces${freshSearch(filterSearch(next), page.url.searchParams, extra)}`, {
			keepFocus: true
		});
	}

	// The peek panel (spec 008): the trace a row opened, beside the listing
	// that opened it, walking that listing's order (spec 009 #13).
	const peekID = $derived(readPeek(page.url.searchParams).peek);
	const selectedObs = $derived(page.url.searchParams.get('obs'));
	let peeked = $state.raw<Trace | null>(null);

	const walk = new Walk(listing, {
		key: (row) => row.timestamp ?? '',
		peekID: () => peekID,
		showing: () =>
			peeked?.id && peeked.timestamp ? { id: peeked.id, key: peeked.timestamp } : null,
		open: peek
	});

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
<svelte:document onvisibilitychange={() => live && !document.hidden && listing.tick()} />

<PageHeader title="Traces">
	{#snippet meta()}
		{#if listing.loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else if listing.total}
			<span class="tabular-nums">{count(listing.total.value)}{listing.total.capped ? '+' : ''}</span>
		{:else}
			<!-- The count has not landed, or could not be taken: what is on
			     screen is still a number, and an empty slot is not (PR #11). -->
			<span class="tabular-nums">{count(listing.rows.length)}</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<!-- Not disabled when paused: `live=1` survives a page turn, and
		     disabling the toggle would be disabling the only control that can
		     unset it (PR #11 review). It says paused and still switches off. -->
		<Button
			variant={live && listing.newest ? 'primary' : 'default'}
			onclick={() => navigate(filters, !live)}
			aria-pressed={live}
			title={live && !listing.newest
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
	<!-- The bar stays on an empty page that is not the first one: a cursor
	     whose rows are gone — swept by retention, say — would otherwise leave
	     no way back to the listing but editing the URL (PR #11 review). -->
	<TraceTable rows={listing.rows} onopen={peek} selectedID={peekID} />
	<PaginationBar {...listing.bar} noun="trace" />
	{#if listing.rows.length === 0 && !listing.loading}
		<p class="text-subtle flex flex-1 items-start justify-center p-8 text-center">
			Nothing on this page any more. Use « to go back to the newest.
		</p>
	{/if}
{:else if !listing.loading && !listing.failure}
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
		onprev={() => walk.step(-1)}
		onnext={() => walk.step(1)}
		hasPrev={walk.hasPrev}
		hasNext={walk.hasNext}
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
