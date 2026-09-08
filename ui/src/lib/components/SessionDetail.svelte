<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { api, type Session, type TraceRow } from '$lib/api/client.svelte';
	import { ABSENT, cost, count, timestamp } from '$lib/format';
	import { asPage, Listing, StateSpot } from '$lib/listing.svelte';
	import { Scores } from '$lib/scores.svelte';
	import ListingShell from './ListingShell.svelte';
	import ScoresBlock from './scores/ScoresBlock.svelte';
	import TraceTable from './TraceTable.svelte';

	// One session: the totals `GET /api/v1/sessions/{id}` returns, over its
	// traces rendered with the Traces screen's own table — a row opens the
	// trace, which is where a session's story actually is.
	//
	// The body only, shared by the full page and the peek panel (spec 008 #8).
	// Where a trace row leads is the caller's to say: the page opens it in a
	// panel over itself, and a session panel drills into it in place (#9).

	let {
		sessionID,
		session = $bindable(null),
		traces = $bindable([]),
		onopen,
		selectedTraceID = null
	}: {
		sessionID: string;
		session?: Session | null;
		/** The rows loaded so far, for a caller that walks them (spec 008 #7). */
		traces?: TraceRow[];
		onopen: (traceID: string) => void;
		selectedTraceID?: string | null;
	} = $props();

	// The one listing that pages in component state rather than in the URL
	// (spec 009 #10), and the one that needs no count of its own: `trace_count`
	// is exact and already on screen (spec 010, divergence 6).
	const listing = new Listing<TraceRow>({
		key: () => sessionID,
		spot: new StateSpot(() => sessionID),
		count: false,
		read: async (at, counting, signal) => {
			try {
				const answer = await api.getSession(sessionID, asPage(at, counting), signal);
				// An aborted answer lands on nothing, this binding included: the
				// body can parse in the window between the abort and this line,
				// and the session it names is then the one being left (#8).
				if (!signal.aborted) session = answer;
				return { ...answer, rows: answer.traces };
			} catch (cause) {
				// The failure below renders instead of the totals, and the panel
				// around it must not go on showing a session that did not arrive.
				if (!signal.aborted) session = null;
				throw cause;
			}
		},
		failed: 'Failed to read the session.'
	});

	// The rows, for a caller that walks them: the loader empties them on a
	// failure and leaves them alone on an abort, which is what the walk needs.
	$effect(() => {
		traces = listing.rows;
	});

	// A session's own scores — a CSAT rating, a conversation-level verdict —
	// over `?session_id=` (spec 022 #2). Each trace's own are one click away,
	// on the trace, which is what they grade.
	const scores = new Scores(() => ({ session_id: sessionID }));
	$effect(() => scores.watch());

	/** The totals header, as label/value pairs so one loop renders them. */
	const totals = $derived(
		session
			? [
					{ label: 'Traces', value: count(session.trace_count) },
					{ label: 'With errors', value: count(session.error_count) },
					{ label: 'Cost', value: cost(session.total_cost) },
					{ label: 'First seen', value: timestamp(session.first_seen) },
					{ label: 'Last seen', value: timestamp(session.last_seen) }
				]
			: []
	);
</script>

<!-- The spinner is for a session that is not on screen yet, not for a page of
     one that is: turning a page keeps the rows and dims the bar, which is what
     the other two listings do and what makes `busy` mean anything here at all
     (spec 009 #8; PR #11, seventh review). -->
{#if listing.loading && session?.id !== sessionID}
	<div class="text-subtle flex flex-1 items-center justify-center gap-2">
		<LoaderCircle class="size-4 animate-spin" />
		Loading the session
	</div>
{:else if listing.failure}
	<div class="flex flex-1 items-start justify-center p-8">
		<p role="alert" class="text-danger flex max-w-md items-start gap-2">
			<TriangleAlert class="mt-0.5 size-4 shrink-0" />
			{listing.failure}
		</p>
	</div>
{:else if session}
	<dl class="border-border flex shrink-0 flex-wrap gap-x-8 gap-y-2 border-b px-4 py-3">
		{#each totals as total (total.label)}
			<div>
				<dt class="text-subtle text-xs">{total.label}</dt>
				<dd class="tabular-nums">{total.value}</dd>
			</div>
		{/each}
	</dl>

	<ScoresBlock
		scores={scores.rows.map((score) => ({ score, unknown: false }))}
		target={{ session_id: sessionID }}
		configs={scores.configs}
		loading={scores.loading}
		failure={scores.failure}
		truncated={scores.more}
		onchanged={() => scores.refresh()}
	/>

	<!-- The total here is exact and already known: `trace_count` is what the
	     endpoint answers with, so this listing asks for no count of its own.
	     No failure line either: a failed session is the branch above, and the
	     panel around this one must not show a table under a banner. -->
	<ListingShell
		{listing}
		noun="trace"
		total={{ value: session.trace_count, capped: false }}
		problem={null}
	>
		{#snippet table()}
			<!-- The session id stays text here: every row of this table belongs to
			     the session already on screen, and a link to where the reader
			     stands is not a destination — from a panel it would tear down the
			     listing behind it to arrive at it (spec 023 #17). -->
			<TraceTable
				rows={listing.rows}
				{onopen}
				selectedID={selectedTraceID}
				linkSession={false}
			/>
		{/snippet}
		{#snippet empty()}
			<p class="text-subtle p-8 text-center">{ABSENT} This session holds no traces.</p>
		{/snippet}
	</ListingShell>
{/if}
