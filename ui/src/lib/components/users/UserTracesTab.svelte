<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type Trace, type TraceRow } from '$lib/api/client.svelte';
	import { asPage, Listing, UrlSpot, Walk } from '$lib/listing.svelte';
	import { peekSearch, readPeek } from '$lib/peek';
	import { href } from '$lib/project.svelte';
	import PaginationBar from '../PaginationBar.svelte';
	import PeekPanel from '../PeekPanel.svelte';
	import TraceDetail from '../TraceDetail.svelte';
	import TracePeekMeta from '../TracePeekMeta.svelte';
	import TraceTable from '../TraceTable.svelte';

	// One user's traces, which is the Traces screen's own table under the
	// shared loader (spec 010, spec 016 #3) with one filter pinned. Reusing the
	// table rather than embedding the page is what brings the peek panel and
	// its walk along; the ⤢ beside the tab is the full listing.

	let { userID }: { userID: string } = $props();

	const filters = $derived({ user_id: userID });
	const listing = new Listing<TraceRow>({
		key: () => userID,
		spot: new UrlSpot(),
		read: async (at, counting, signal) => {
			const answer = await api.listTraces(filters, asPage(at, counting), signal);
			return { ...answer, rows: answer.traces };
		},
		failed: 'Failed to read this user’s traces.'
	});

	const peekID = $derived(readPeek(page.url.searchParams).peek);
	const selectedObs = $derived(page.url.searchParams.get('obs'));
	let peeked = $state.raw<Trace | null>(null);

	const walk = new Walk(listing, {
		key: (row) => row.timestamp ?? '',
		peekID: () => peekID,
		showing: () => (peeked?.id && peeked.timestamp ? { id: peeked.id, key: peeked.timestamp } : null),
		open: (id) => peek(id)
	});

	/** Opening pushes one entry; moving between rows replaces it (spec 008 #6). */
	function peek(traceID: string | null, observationID?: string | null) {
		const search = peekSearch(page.url.searchParams, { peek: traceID, obs: observationID });
		goto(`${page.url.pathname}${search}`, {
			replaceState: traceID === null || peekID !== null,
			keepFocus: true,
			noScroll: true
		});
	}
</script>

{#if listing.problem}
	<p role="alert" class="text-danger flex items-center gap-2 px-4 py-2">
		<TriangleAlert class="size-4 shrink-0" />
		{listing.problem}
	</p>
{/if}

<TraceTable rows={listing.rows} onopen={peek} selectedID={peekID} />
<PaginationBar {...listing.bar} noun="trace" />

{#if peekID}
	<PeekPanel
		label="Trace"
		onclose={() => peek(null)}
		onprev={() => walk.step(-1)}
		onnext={() => walk.step(1)}
		hasPrev={walk.hasPrev}
		hasNext={walk.hasNext}
		fullHref={href(
			`/traces/${encodeURIComponent(peekID)}${selectedObs ? `?obs=${encodeURIComponent(selectedObs)}` : ''}`
		)}
		fullLabel="Open this trace as a page"
	>
		{#snippet title()}
			<h2 class="truncate text-lg font-semibold tracking-tight">{peeked?.name ?? 'Trace'}</h2>
		{/snippet}
		{#snippet meta()}
			<!-- No release and no id span: the tab is narrow, and the page around
			     it already says whose traces these are. -->
			<TracePeekMeta trace={peeked} hide={['release', 'id']} />
		{/snippet}
		<TraceDetail traceID={peekID} bind:trace={peeked} />
	</PeekPanel>
{/if}
