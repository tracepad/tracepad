<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api, type Observation, type Trace } from '$lib/api/client.svelte';
	import { ABSENT } from '$lib/format';
	import { observationIDs, splitScores } from '$lib/scores';
	import { Scores } from '$lib/scores.svelte';
	import JsonView from './json/JsonView.svelte';
	import ObservationDetail from './ObservationDetail.svelte';
	import ScoresBlock from './scores/ScoresBlock.svelte';
	import TraceTree from './TraceTree.svelte';

	// The trace as a tree on the left and the selected observation on the
	// right, both of them views of one `GET /api/v1/traces/{id}?expand=io`.
	// The selection is in the URL, so a link points at an observation and not
	// merely at a trace (spec 006, Application contract).
	//
	// The body only: the full page and the peek panel render this same
	// component and wear their own chrome above it (spec 008 #8), which is why
	// the trace it loaded is handed back for that chrome to name.

	let {
		traceID,
		trace = $bindable(null)
	}: { traceID: string; trace?: Trace | null } = $props();

	const selectedID = $derived(page.url.searchParams.get('obs'));

	let loading = $state(true);
	let failure = $state<string | null>(null);
	// Which pane a phone is showing; on a wide screen both are visible at once
	// (spec 006 #15).
	// A link that names an observation opens on it; one that names only a
	// trace opens on the tree.
	const paneForURL = (): 'tree' | 'detail' =>
		page.url.searchParams.get('obs') ? 'detail' : 'tree';
	let pane = $state<'tree' | 'detail'>(untrack(paneForURL));

	$effect(() => {
		const wanted = traceID;
		const controller = new AbortController();
		load(wanted, controller.signal);
		return () => controller.abort();
	});

	// One read beside the trace answers both surfaces (spec 022 #1): the
	// header takes the scores of the trace itself, each observation panel
	// takes its own, and the tree counts them. Splitting one loaded document
	// by a field is rendering, not the client logic spec 004 #1 forbids.
	const scores = new Scores(() => ({ trace_id: traceID }));
	$effect(() => scores.watch());

	const split = $derived(splitScores(scores.rows, observationIDs(trace?.observations)));
	/** What the header says about the ones it is not showing (#1). */
	const elsewhere = $derived(
		split.onObservations === 0
			? null
			: `${split.onObservations} more on observation${split.onObservations === 1 ? '' : 's'}`
	);

	$effect(() => {
		// One component serves every trace — the route reuses it across ids,
		// and the panel swaps ids without unmounting — so the pane the reader
		// chose for the previous trace would otherwise carry over, landing a
		// phone on the detail of an observation nobody picked. Depends on the
		// id alone: choosing an observation within one trace must not undo the
		// choice of pane.
		traceID;
		untrack(() => (pane = paneForURL()));
	});

	async function load(wanted: string, signal: AbortSignal) {
		loading = true;
		failure = null;
		// Dropped before the request, not after it: the caller names this
		// trace in its own chrome, and keeping the last one there would put
		// the previous trace's timestamp, cost and copyable id beside an
		// expand link that already points at the new one (PR #10 review).
		trace = null;
		try {
			trace = await api.getTrace(wanted, signal);
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
	const selected = $derived((selectedID ? find(roots, selectedID) : null) ?? roots[0] ?? null);
	// The whole trace was too wide for the budget, so no payload was inlined
	// anywhere (spec 004 #31); the detail panel offers to fetch them one
	// observation at a time.
	const refused = $derived(trace?.expansion?.expanded === false);

	// What the exporter said about the run as a whole, as opposed to about any
	// one span. It belongs under the tree because that is the pane that is
	// about the trace; the pane beside it is about one observation.
	const traceMetadata = $derived.by(() => {
		const metadata = trace?.metadata;
		return metadata && Object.keys(metadata).length > 0 ? metadata : null;
	});

	function select(observationID: string, activate: boolean) {
		const search = new URLSearchParams(page.url.searchParams);
		search.set('obs', observationID);
		goto(`?${search}`, { replaceState: true, keepFocus: true, noScroll: true });
		if (activate) pane = 'detail';
	}
</script>

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
		<p class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2">
			<TriangleAlert class="size-4 shrink-0" />
			{trace.error_count}
			{trace.error_count === 1 ? 'observation' : 'observations'} failed in this trace
		</p>
	{/if}

	<ScoresBlock
		scores={split.header}
		target={{ trace_id: trace.id }}
		configs={scores.configs}
		note={elsewhere}
		loading={scores.loading}
		failure={scores.failure}
		truncated={scores.more}
		onchanged={() => scores.refresh()}
	/>

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
			<TraceTree
				observations={roots}
				selectedID={selected?.id ?? null}
				onselect={select}
				scored={split.byObservation}
			/>
			{#if traceMetadata}
				<section class="border-border shrink-0 border-t px-3 py-2 [&_.cm-editor]:max-h-52">
					<h3 class="text-muted mb-1.5 text-xs font-medium tracking-wide uppercase">Metadata</h3>
					<JsonView value={traceMetadata} label="Trace metadata" />
				</section>
			{/if}
		</div>
		<div
			class={[
				'min-h-0 min-w-0 flex-1 md:flex md:flex-col',
				pane === 'detail' ? 'flex flex-col' : 'hidden'
			]}
		>
			{#if selected}
				{#key selected.id}
					<ObservationDetail
						observation={selected}
						traceID={trace.id}
						{refused}
						scores={split.byObservation.get(selected.id) ?? []}
						configs={scores.configs}
						onscored={() => scores.refresh()}
					/>
				{/key}
			{:else}
				<p class="text-subtle p-8 text-center">
					{ABSENT} This trace carries no observations.
				</p>
			{/if}
		</div>
	</div>
{/if}
