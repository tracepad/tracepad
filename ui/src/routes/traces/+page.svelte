<script lang="ts">
	import Inbox from '@lucide/svelte/icons/inbox';
	import Pause from '@lucide/svelte/icons/pause';
	import Play from '@lucide/svelte/icons/play';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type Trace, type TraceRow } from '$lib/api/client.svelte';
	import { filterCount, filterSearch, readFilters, type TraceFilters } from '$lib/api/traces';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import FilterBar from '$lib/components/FilterBar.svelte';
	import ListingCount from '$lib/components/ListingCount.svelte';
	import ListingShell from '$lib/components/ListingShell.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PeekPanel from '$lib/components/PeekPanel.svelte';
	import AddToQueue from '$lib/components/queues/AddToQueue.svelte';
	import TraceDetail from '$lib/components/TraceDetail.svelte';
	import TracePeekMeta from '$lib/components/TracePeekMeta.svelte';
	import TraceTable from '$lib/components/TraceTable.svelte';
	import { asPage, Listing, UrlSpot, Walk } from '$lib/listing.svelte';
	import { freshSearch } from '$lib/page';
	import { peekSearch, readPeek } from '$lib/peek';
	import { queueable } from '$lib/queues';

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

	/**
	 * Opening pushes one entry; moving between rows replaces it (spec 008 #6).
	 * A row opened out of a search opens on the observation that matched, which
	 * is the whole point of the row saying where it did (spec 011 #6).
	 */
	function peek(id: string | null, observationID: string | null = null) {
		const search = peekSearch(page.url.searchParams, { peek: id, obs: observationID });
		goto(`${page.url.pathname}${search}`, {
			replaceState: id === null || peekID !== null,
			keepFocus: true,
			noScroll: true
		});
	}

	// What *Add to queue…* may take (spec 024 #13). The count is the one the
	// listing already holds, so the number is on screen before the call — and
	// a filter matching more than the endpoint's cap is refused with the
	// reason rather than silently truncated to the newest thousand.
	const takeable = $derived(queueable(listing.total));

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
		<ListingCount {listing} />
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

<div class="border-border flex items-center gap-2 overflow-x-auto border-b px-4 py-2">
	<FilterBar {filters} onchange={(next) => navigate(next)} />
	<!-- The manager's gesture, at the surface where the choice is made: this
	     filtered list is what deserves a human verdict (spec 024 #13). -->
	<AddToQueue
		{filters}
		matched={takeable.label}
		blocked={takeable.blocked}
		label="Add to queue…"
	/>
</div>

<ListingShell {listing} noun="trace">
	{#snippet table()}
		<TraceTable rows={listing.rows} onopen={peek} selectedID={peekID} search={filters.q ?? ''} />
	{/snippet}
	{#snippet empty()}
		<div class="flex flex-1 items-start justify-center overflow-auto p-8">
			<div class="max-w-lg">
				{#if filters.q}
					<!-- The query is named back, because "nothing matches" is only
					     useful when it says what found nothing (spec 011). -->
					<h2 class="font-medium">Nothing matches “{filters.q}”</h2>
					<p class="text-muted mt-1">
						Search finds whole words, not parts of them: <code class="font-mono">err</code> does not
						find <code class="font-mono">errors</code>, <code class="font-mono">err*</code> does.
						Quote words to keep them together.
					</p>
					<div class="mt-3 flex gap-2">
						{#if filterCount(filters) > 1}
							<Button onclick={() => navigate({ q: filters.q })}>Keep the search, clear filters</Button>
						{/if}
						<Button onclick={() => navigate({ ...filters, q: undefined })}>Clear the search</Button>
					</div>
				{:else if filtering}
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
	{/snippet}
</ListingShell>

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
			<TracePeekMeta trace={peeked} />
		{/snippet}
		<TraceDetail traceID={peekID} bind:trace={peeked} />
	</PeekPanel>
{/if}
