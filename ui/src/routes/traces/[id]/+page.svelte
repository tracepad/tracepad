<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api, type Observation, type Trace } from '$lib/api/client.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import ObservationDetail from '$lib/components/ObservationDetail.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import TraceTree from '$lib/components/TraceTree.svelte';
	import { ABSENT, cost, duration, timestamp } from '$lib/format';

	// The trace as a tree on the left and the selected observation on the
	// right, both of them views of one `GET /api/v1/traces/{id}?expand=io`.
	// The selection is in the URL, so a link points at an observation and not
	// merely at a trace (Application contract).

	const id = $derived(page.params.id ?? '');
	const selectedID = $derived(page.url.searchParams.get('obs'));

	let trace = $state.raw<Trace | null>(null);
	let loading = $state(true);
	let failure = $state<string | null>(null);
	// Which pane a phone is showing; on a wide screen both are visible at once
	// (spec 006 #15).
	let pane = $state<'tree' | 'detail'>('tree');

	$effect(() => {
		const traceID = id;
		const controller = new AbortController();
		load(traceID, controller.signal);
		return () => controller.abort();
	});

	async function load(traceID: string, signal: AbortSignal) {
		loading = true;
		failure = null;
		try {
			trace = await api.getTrace(traceID, signal);
		} catch (cause) {
			if (signal.aborted) return;
			trace = null;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the trace.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	function find(nodes: Observation[] | undefined, wanted: string): Observation | null {
		for (const node of nodes ?? []) {
			if (node.id === wanted) return node;
			const inside = find(node.children, wanted);
			if (inside) return inside;
		}
		return null;
	}

	const roots = $derived(trace?.observations ?? []);
	// A trace opened without `?obs=` shows its first observation rather than an
	// empty panel: the first span is what the trace starts as.
	const selected = $derived(
		(selectedID ? find(roots, selectedID) : null) ?? roots[0] ?? null
	);
	// The whole trace was too wide for the budget, so no payload was inlined
	// anywhere (spec 004 #31); the detail panel offers to fetch them one
	// observation at a time.
	const refused = $derived(trace?.expansion?.expanded === false);

	function select(observationID: string) {
		const search = new URLSearchParams(page.url.searchParams);
		search.set('obs', observationID);
		goto(`?${search}`, { replaceState: true, keepFocus: true, noScroll: true });
		pane = 'detail';
	}
</script>

<svelte:head><title>{trace?.name ?? 'Trace'} · Tracepad</title></svelte:head>

<PageHeader title={trace?.name ?? 'Trace'}>
	{#snippet meta()}
		<a href="/traces" class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			Traces
		</a>
		{#if trace}
			<span class="hidden font-mono sm:inline">{timestamp(trace.timestamp)}</span>
			<span class="hidden tabular-nums md:inline">{duration(trace.latency_ms)}</span>
			<span class="hidden tabular-nums md:inline">{cost(trace.total_cost)}</span>
			<span class="hidden truncate font-mono lg:inline">{trace.id}</span>
			<CopyButton text={trace.id} label="Copy the trace id" />
		{/if}
	{/snippet}
</PageHeader>

{#if loading}
	<div class="text-subtle flex flex-1 items-center justify-center gap-2">
		<LoaderCircle class="size-4 animate-spin" />
		Loading the trace
	</div>
{:else if failure}
	<div class="flex flex-1 items-start justify-center p-8">
		<p role="alert" class="text-danger flex max-w-md items-start gap-2">
			<TriangleAlert class="mt-0.5 size-4 shrink-0" />
			{failure}
		</p>
	</div>
{:else if trace}
	{#if trace.error_count > 0}
		<p
			class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
		>
			<TriangleAlert class="size-4 shrink-0" />
			{trace.error_count}
			{trace.error_count === 1 ? 'observation' : 'observations'} failed in this trace
		</p>
	{/if}

	<!-- Two panes side by side; on a phone one at a time, switched here. -->
	<div class="border-border flex shrink-0 gap-1 border-b px-3 py-1.5 md:hidden" role="tablist">
		{#each [['tree', 'Tree'], ['detail', 'Observation']] as const as [value, label] (value)}
			<button
				type="button"
				role="tab"
				aria-selected={pane === value}
				onclick={() => (pane = value)}
				class={[
					'pointer-coarse:min-h-11 flex-1 cursor-pointer rounded-md px-3 py-1.5 font-medium',
					pane === value ? 'bg-accent-soft text-accent' : 'text-muted hover:bg-raised'
				]}
			>
				{label}
			</button>
		{/each}
	</div>

	<div class="flex min-h-0 flex-1 md:flex-row">
		<div
			class={[
				'border-border flex min-h-0 min-w-0 flex-col md:flex md:w-2/5 md:max-w-lg md:border-r',
				pane === 'tree' ? 'flex flex-1' : 'hidden'
			]}
		>
			<TraceTree observations={roots} selectedID={selected?.id ?? null} onselect={select} />
		</div>
		<div
			class={[
				'min-h-0 min-w-0 flex-1 md:flex md:flex-col',
				pane === 'detail' ? 'flex flex-col' : 'hidden'
			]}
		>
			{#if selected}
				{#key selected.id}
					<ObservationDetail observation={selected} traceID={trace.id} {refused} />
				{/key}
			{:else}
				<p class="text-subtle p-8 text-center">
					{ABSENT} This trace carries no observations.
				</p>
			{/if}
		</div>
	</div>
{/if}
