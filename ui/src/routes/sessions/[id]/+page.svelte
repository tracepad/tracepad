<script lang="ts">
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { page } from '$app/state';
	import { ApiError, api, type Session, type TraceRow } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import TraceTable from '$lib/components/TraceTable.svelte';
	import { ABSENT, cost, count, timestamp } from '$lib/format';

	// One session: the totals `GET /api/v1/sessions/{id}` returns, over its
	// traces rendered with the Traces screen's own table — a row opens the
	// trace, which is where a session's story actually is.

	const PAGE_SIZE = 50;

	const id = $derived(page.params.id ?? '');

	let session = $state.raw<Session | null>(null);
	let traces = $state.raw<TraceRow[]>([]);
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
		const sessionID = id;
		const controller = new AbortController();
		query = controller;
		load(sessionID, controller.signal);
		return () => controller.abort();
	});

	async function load(sessionID: string, signal: AbortSignal) {
		loading = true;
		loadingMore = false;
		failure = null;
		moreFailure = null;
		try {
			const answer = await api.getSession(sessionID, { limit: PAGE_SIZE }, signal);
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
			const answer = await api.getSession(id, { limit: PAGE_SIZE, cursor }, signal);
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
		<TraceTable rows={traces} />
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
