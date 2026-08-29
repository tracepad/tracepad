<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { untrack } from 'svelte';
	import { ApiError, api, type Session, type TraceRow } from '$lib/api/client.svelte';
	import { ABSENT, cost, count, timestamp } from '$lib/format';
	import { DEFAULT_PAGE_SIZE, isFirstPage, isLastPage, type PageState } from '$lib/page';
	import PaginationBar from './PaginationBar.svelte';
	import TraceTable from './TraceTable.svelte';

	// One session: the totals `GET /api/v1/sessions/{id}` returns, over its
	// traces rendered with the Traces screen's own table — a row opens the
	// trace, which is where a session's story actually is.
	//
	// The body only, shared by the full page and the peek panel (spec 008 #8).
	// Where a trace row leads is the caller's to say: the page opens it in a
	// panel over itself, and a session panel drills into it in place (#9).

	/**
	 * The one listing that pages in component state rather than in the URL
	 * (spec 009 #10): on `/sessions` this table lives inside the peek panel,
	 * over a URL whose `limit` and `cursor` already belong to the listing
	 * behind it, and two listings on one address cannot own one set of keys.
	 */
	let chosen = $state.raw<PageState & { session: string | null }>({
		session: null,
		limit: DEFAULT_PAGE_SIZE,
		cursor: null,
		direction: 'next'
	});

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
	let nextCursor = $state.raw<string | null>(null);
	let prevCursor = $state.raw<string | null>(null);
	let loading = $state(true);
	let failure = $state<string | null>(null);

	/**
	 * The page in force. Derived rather than reset by an effect: a cursor
	 * belongs to the session it was taken from, so one naming another session
	 * simply is not the page — no write, no second render, no second request.
	 */
	const spot = $derived<PageState>(
		chosen.session === sessionID
			? chosen
			: { limit: chosen.limit, cursor: null, direction: 'next' }
	);

	$effect(() => {
		// A different session starts at its newest page; the same session
		// re-reads whenever the page moves.
		const wanted = sessionID;
		const at = spot;
		const controller = new AbortController();
		load(wanted, at, controller.signal);
		return () => controller.abort();
	});

	async function load(wanted: string, at: PageState, signal: AbortSignal) {
		loading = true;
		failure = null;
		try {
			const answer = await api.getSession(
				wanted,
				{ limit: at.limit, cursor: at.cursor ?? undefined, direction: at.direction },
				signal
			);
			session = answer;
			traces = answer.traces;
			nextCursor = answer.next_cursor;
			prevCursor = answer.prev_cursor;
		} catch (cause) {
			if (signal.aborted) return;
			session = null;
			traces = [];
			nextCursor = prevCursor = null;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the session.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	function turn(to: Partial<PageState>) {
		chosen = {
			session: sessionID,
			limit: spot.limit,
			cursor: null,
			direction: 'next',
			...to
		};
	}

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

{#if loading}
	<div class="text-subtle flex flex-1 items-center justify-center gap-2">
		<LoaderCircle class="size-4 animate-spin" />
		Loading the session
	</div>
{:else if failure}
	<div class="flex flex-1 items-start justify-center p-8">
		<p role="alert" class="text-danger flex max-w-md items-start gap-2">
			<TriangleAlert class="mt-0.5 size-4 shrink-0" />
			{failure}
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

	{#if traces.length > 0 || !isFirstPage(spot)}
		<TraceTable rows={traces} {onopen} selectedID={selectedTraceID} />
		<!-- The total here is exact and already known: `trace_count` is what
		     the endpoint answers with, so this listing needs no count of its
		     own (spec 009, API contract). -->
		<PaginationBar
			limit={spot.limit}
			rows={traces.length}
			total={{ value: session.trace_count, capped: false }}
			hasPrev={prevCursor !== null}
			hasNext={nextCursor !== null}
			busy={loading}
			atNewest={isFirstPage(spot)}
			atOldest={isLastPage(spot)}
			onresize={(limit) => turn({ limit })}
			onfirst={() => turn({})}
			onprev={() => turn({ cursor: prevCursor ?? undefined, direction: 'prev' })}
			onnext={() => turn({ cursor: nextCursor ?? undefined })}
			onlast={() => turn({ direction: 'prev' })}
			noun="trace"
		/>
		{#if traces.length === 0 && !loading}
			<p class="text-subtle p-8 text-center">
				Nothing on this page any more. Use « to go back to the newest.
			</p>
		{/if}
	{:else}
		<p class="text-subtle p-8 text-center">{ABSENT} This session holds no traces.</p>
	{/if}
{/if}
