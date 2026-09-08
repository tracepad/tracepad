<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { type Session, type Trace, type TraceRow } from '$lib/api/client.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PeekPanel from '$lib/components/PeekPanel.svelte';
	import SessionDetail from '$lib/components/SessionDetail.svelte';
	import TraceDetail from '$lib/components/TraceDetail.svelte';
	import TracePeekMeta from '$lib/components/TracePeekMeta.svelte';
	import { anchor, neighbour, peekSearch, readPeek } from '$lib/peek';

	// The full page of one session: the shell's header over the same body the
	// peek panel shows (spec 008 #8). Its trace table is a listing like any
	// other, so a row opens in a panel over it rather than navigating away
	// (#10).

	const id = $derived(page.params.id ?? '');

	// Both loaded by the body below: the session for the header, the rows for
	// the panel's previous/next.
	let session = $state.raw<Session | null>(null);
	let traces = $state.raw<TraceRow[]>([]);

	const peekID = $derived(readPeek(page.url.searchParams).peek);
	const selectedObs = $derived(page.url.searchParams.get('obs'));
	let peeked = $state.raw<Trace | null>(null);

	// By order, not by index: this table pages too (spec 009 #10), and a turn
	// with the panel open leaves the peeked trace off the page.
	const ordered = $derived(traces.map((row) => ({ id: row.id, key: row.timestamp ?? '' })));
	const showing = $derived(
		peeked?.id && peeked.timestamp ? { id: peeked.id, key: peeked.timestamp } : null
	);
	const position = $derived(anchor(ordered, peekID, showing));
	const previous = $derived(neighbour(ordered, position, -1));
	const following = $derived(neighbour(ordered, position, 1));

	/** Opening pushes one entry; moving between rows replaces it (#6). */
	function peek(traceID: string | null) {
		const search = peekSearch(page.url.searchParams, { peek: traceID });
		goto(`${page.url.pathname}${search}`, {
			replaceState: traceID === null || peekID !== null,
			keepFocus: true,
			noScroll: true
		});
	}
</script>

<svelte:head><title>{id} · Sessions · Tracepad</title></svelte:head>

<PageHeader title="Session">
	{#snippet meta()}
		<a href="/sessions" class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			Sessions
		</a>
		<span class="truncate font-mono">{id}</span>
		<CopyButton text={id} label="Copy the session id" />
	{/snippet}
</PageHeader>

<SessionDetail
	sessionID={id}
	bind:session
	bind:traces
	onopen={peek}
	selectedTraceID={peekID}
/>

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
			<h2 class="truncate text-lg font-semibold tracking-tight">{peeked?.name ?? 'Trace'}</h2>
		{/snippet}
		{#snippet meta()}
			<!-- No id span: the header above this panel already carries one, and
			     it is the session's. -->
			<TracePeekMeta trace={peeked} hide={['id']} />
		{/snippet}
		<TraceDetail traceID={peekID} bind:trace={peeked} />
	</PeekPanel>
{/if}
