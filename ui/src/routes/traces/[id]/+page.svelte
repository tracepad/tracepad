<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import { page } from '$app/state';
	import { type Trace } from '$lib/api/client.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import TraceDetail from '$lib/components/TraceDetail.svelte';
	import { cost, duration, timestamp } from '$lib/format';

	// The full page of one trace: the shell's header over the same body the
	// peek panel shows (spec 008 #8). This is the canonical, shareable form —
	// the link the panel's ⤢ control leads to, and the one a row still carries
	// for a new tab (#3).

	const id = $derived(page.params.id ?? '');

	// Loaded by the body below; the header is the only reason it is up here.
	let trace = $state.raw<Trace | null>(null);
</script>

<svelte:head><title>{trace?.name ?? 'Trace'} · Tracepad</title></svelte:head>

<PageHeader title={trace?.name ?? 'Trace'}>
	{#snippet meta()}
		<a href="/traces" class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			Traces
		</a>
		{#if trace}
			<!-- The short parts do not shrink; the release and the two ids, which
			     carry `truncate`, are what gives way. `TracePeekMeta` carries the
			     same rule, and this header carries it by hand: that is the price
			     of not folding the two together (spec 023 #17c). -->
			<span class="hidden shrink-0 font-mono sm:inline">{timestamp(trace.timestamp)}</span>
			<!-- The release, when the trace named one: it is a filter rather
			     than a column, so the header is where a reader finds out which
			     deployment they are looking at (spec 012, Application
			     contract). -->
			{#if trace.release}
				<span class="hidden truncate md:inline" title="Release">{trace.release}</span>
			{/if}
			<!-- Whose trace this is, and a way to everything else they did
			     (spec 023, Application contract). -->
			{#if trace.user_id}
				<a
					href="/users/{encodeURIComponent(trace.user_id)}"
					title="Everything about {trace.user_id}"
					class="hover:text-fg hidden truncate font-mono hover:underline md:inline"
				>
					{trace.user_id}
				</a>
			{/if}
			<!-- And which session it belongs to, which the header never said at
			     all until spec 023 #16. -->
			{#if trace.session_id}
				<a
					href="/sessions/{encodeURIComponent(trace.session_id)}"
					title="Everything in {trace.session_id}"
					class="hover:text-fg hidden truncate font-mono hover:underline md:inline"
				>
					{trace.session_id}
				</a>
			{/if}
			<span class="hidden shrink-0 tabular-nums md:inline">{duration(trace.latency_ms)}</span>
			<span class="hidden shrink-0 tabular-nums md:inline">{cost(trace.total_cost)}</span>
			<span class="hidden truncate font-mono lg:inline">{trace.id}</span>
			<CopyButton text={trace.id} label="Copy the trace id" />
		{/if}
	{/snippet}
</PageHeader>

<TraceDetail traceID={id} bind:trace />
