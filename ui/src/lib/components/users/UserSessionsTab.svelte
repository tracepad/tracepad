<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type SessionRow } from '$lib/api/client.svelte';
	import { asPage, Listing, UrlSpot, Walk } from '$lib/listing.svelte';
	import { peekSearch, readPeek } from '$lib/peek';
	import CopyButton from '../CopyButton.svelte';
	import PaginationBar from '../PaginationBar.svelte';
	import PeekPanel from '../PeekPanel.svelte';
	import SessionDetail from '../SessionDetail.svelte';
	import SessionTable from '../SessionTable.svelte';

	// One user's sessions: the Sessions screen's table under the shared loader
	// with `user_id` pinned. The panel stops at the session — a trace inside it
	// is a link to its own page, where the Sessions screen drills in place
	// instead (spec 008 #9); one level is what this tab is for.

	let { userID }: { userID: string } = $props();

	const filters = $derived({ user_id: userID });
	const listing = new Listing<SessionRow>({
		key: () => userID,
		spot: new UrlSpot(),
		read: async (at, counting, signal) => {
			const answer = await api.listSessions(filters, asPage(at, counting), signal);
			return { ...answer, rows: answer.sessions };
		},
		failed: 'Failed to read this user’s sessions.'
	});

	const peekID = $derived(readPeek(page.url.searchParams).peek);

	const walk = new Walk(listing, {
		// Under a filter `last_seen` is the span of the traces that matched,
		// not the session's own, so the panel's row is placed only from the
		// page it is on (spec 009 #14).
		key: (row) => row.last_seen ?? '',
		peekID: () => peekID,
		showing: () => null,
		open: (id) => peek(id)
	});

	function peek(sessionID: string | null) {
		const search = peekSearch(page.url.searchParams, { peek: sessionID });
		goto(`${page.url.pathname}${search}`, {
			replaceState: sessionID === null || peekID !== null,
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

<SessionTable rows={listing.rows} onopen={peek} selectedID={peekID} />
<PaginationBar {...listing.bar} noun="session" />

{#if peekID}
	<PeekPanel
		label="Session"
		onclose={() => peek(null)}
		onprev={() => walk.step(-1)}
		onnext={() => walk.step(1)}
		hasPrev={walk.hasPrev}
		hasNext={walk.hasNext}
		fullHref="/sessions/{encodeURIComponent(peekID)}"
		fullLabel="Open this session as a page"
	>
		{#snippet title()}
			<h2 class="shrink-0 text-lg font-semibold tracking-tight">Session</h2>
		{/snippet}
		{#snippet meta()}
			<span class="truncate font-mono">{peekID}</span>
			<CopyButton text={peekID} label="Copy the session id" />
		{/snippet}
		<SessionDetail
			sessionID={peekID}
			onopen={(traceID) => goto(`/traces/${encodeURIComponent(traceID)}`)}
		/>
	</PeekPanel>
{/if}
