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
			<span class="hidden font-mono sm:inline">{timestamp(trace.timestamp)}</span>
			<!-- The release, when the trace named one: it is a filter rather
			     than a column, so the header is where a reader finds out which
			     deployment they are looking at (spec 012, Application
			     contract). -->
			{#if trace.release}
				<span class="hidden truncate md:inline" title="Release">{trace.release}</span>
			{/if}
			<span class="hidden tabular-nums md:inline">{duration(trace.latency_ms)}</span>
			<span class="hidden tabular-nums md:inline">{cost(trace.total_cost)}</span>
			<span class="hidden truncate font-mono lg:inline">{trace.id}</span>
			<CopyButton text={trace.id} label="Copy the trace id" />
		{/if}
	{/snippet}
</PageHeader>

<TraceDetail traceID={id} bind:trace />
