<script lang="ts">
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { ApiError, api, type Session, type TraceRow } from '$lib/api/client.svelte';
	import { ABSENT, cost, count, timestamp } from '$lib/format';
	import Button from './Button.svelte';
	import TraceTable from './TraceTable.svelte';

	// One session: the totals `GET /api/v1/sessions/{id}` returns, over its
	// traces rendered with the Traces screen's own table — a row opens the
	// trace, which is where a session's story actually is.
	//
	// The body only, shared by the full page and the peek panel (spec 008 #8).
	// Where a trace row leads is the caller's to say: the page opens it in a
	// panel over itself, and a session panel drills into it in place (#9).

	const PAGE_SIZE = 50;

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
	let cursor = $state.raw<string | null>(null);
	let loading = $state(true);
	let loadingMore = $state(false);
	let failure = $state<string | null>(null);
	/**
	 * A page that failed to arrive, kept apart from the one that failed to
	 * load: the session is on screen and readable, and replacing it with an
	 * error line would take away the button needed to try again.
	 */
	let moreFailure = $state<string | null>(null);

	let query: AbortController | null = null;

	$effect(() => {
		const wanted = sessionID;
		const controller = new AbortController();
		query = controller;
		load(wanted, controller.signal);
		return () => controller.abort();
	});

	async function load(wanted: string, signal: AbortSignal) {
		loading = true;
		loadingMore = false;
		failure = null;
		moreFailure = null;
		try {
			const answer = await api.getSession(wanted, { limit: PAGE_SIZE }, signal);
			session = answer;
			traces = answer.traces;
			cursor = answer.next_cursor;
		} catch (cause) {
			if (signal.aborted) return;
			session = null;
			traces = [];
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the session.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	async function loadMore() {
		const controller = query;
		if (!cursor || loadingMore || !controller) return;
		const { signal } = controller;
		loadingMore = true;
		moreFailure = null;
		try {
			const answer = await api.getSession(sessionID, { limit: PAGE_SIZE, cursor }, signal);
			if (signal.aborted) return;
			traces = [...traces, ...answer.traces];
			cursor = answer.next_cursor;
		} catch (cause) {
			if (signal.aborted) return;
			moreFailure = cause instanceof ApiError ? cause.message : 'Failed to read the next page.';
		} finally {
			if (!signal.aborted) loadingMore = false;
		}
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

	{#if traces.length > 0}
		<TraceTable rows={traces} {onopen} selectedID={selectedTraceID} />
		<div
			class="border-border flex shrink-0 flex-col items-center justify-center gap-2 border-t
				px-4 py-2"
		>
			{#if moreFailure}
				<p role="alert" class="text-danger flex items-center gap-2 text-sm">
					<TriangleAlert class="size-4 shrink-0" />
					{moreFailure}
				</p>
			{/if}
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
	{:else}
		<p class="text-subtle p-8 text-center">{ABSENT} This session holds no traces.</p>
	{/if}
{/if}
