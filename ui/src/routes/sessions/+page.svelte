<script lang="ts">
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import MessagesSquare from '@lucide/svelte/icons/messages-square';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api, type SessionRow } from '$lib/api/client.svelte';
	import { readSessionFilters, sessionSearch, type SessionFilters } from '$lib/api/sessions';
	import Button from '$lib/components/Button.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import RangePicker from '$lib/components/RangePicker.svelte';
	import SessionTable from '$lib/components/SessionTable.svelte';
	import { count } from '$lib/format';

	// Sessions over `GET /api/v1/sessions`, the endpoint this spec added for
	// all three clients at once. No live mode here (spec 007 #8): a session's
	// totals move only when its traces do, and the Traces screen is where
	// arrival is watched. This one refetches on a filter change and on the
	// refresh control, which is the whole of its update story.

	const PAGE_SIZE = 50;

	const filters = $derived(readSessionFilters(page.url.searchParams));
	const filtering = $derived(page.url.search !== '');

	let rows = $state.raw<SessionRow[]>([]);
	let cursor = $state.raw<string | null>(null);
	let loading = $state(true);
	let loadingMore = $state(false);
	let failure = $state<string | null>(null);
	/** Bumped by the refresh control to re-run the load effect. */
	let generation = $state(0);

	// Every request on this screen belongs to one set of filters; changing them
	// aborts the lot, so a "load more" in flight cannot append the previous
	// query's page onto the new one (the bug spec 006's review found).
	let query: AbortController | null = null;

	$effect(() => {
		sessionSearch(filters);
		generation;
		const controller = new AbortController();
		query = controller;
		load(controller.signal);
		return () => controller.abort();
	});

	async function load(signal: AbortSignal) {
		loading = true;
		loadingMore = false;
		failure = null;
		try {
			const answer = await api.listSessions(filters, { limit: PAGE_SIZE }, signal);
			rows = answer.sessions;
			cursor = answer.next_cursor;
		} catch (cause) {
			if (signal.aborted) return;
			rows = [];
			failure = describe(cause);
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	async function loadMore() {
		const controller = query;
		if (!cursor || loadingMore || !controller) return;
		const { signal } = controller;
		loadingMore = true;
		failure = null;
		try {
			const answer = await api.listSessions(filters, { limit: PAGE_SIZE, cursor }, signal);
			if (signal.aborted) return;
			rows = [...rows, ...answer.sessions];
			cursor = answer.next_cursor;
		} catch (cause) {
			if (signal.aborted) return;
			failure = describe(cause);
		} finally {
			if (!signal.aborted) loadingMore = false;
		}
	}

	function describe(cause: unknown): string {
		return cause instanceof ApiError ? cause.message : 'Failed to read the sessions.';
	}

	function navigate(next: SessionFilters) {
		goto(`/sessions${sessionSearch(next)}`, { keepFocus: true });
	}

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
		{#if loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else}
			<span class="tabular-nums">{count(rows.length)}{cursor ? '+' : ''}</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button onclick={() => generation++} busy={loading} title="Read the listing again">
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

{#if failure}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{failure}
	</p>
{/if}

{#if rows.length > 0}
	<SessionTable {rows} />
	<div class="border-border flex shrink-0 items-center justify-center border-t px-4 py-2">
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
{:else if !loading && !failure}
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
