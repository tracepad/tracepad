<script lang="ts">
	import type { Trace } from '$lib/api/client.svelte';
	import { cost, duration, timestamp } from '$lib/format';
	import CopyButton from './CopyButton.svelte';

	// What a panel says about the trace it has open (spec 026 #4): when it
	// happened, which release, which session, how long, how much, and the id
	// with the button that copies it — each at the width it earns its place at.
	//
	// The same five spans with the same five breakpoint classes stood in six
	// files, and a site that showed four of them showed them by leaving two
	// lines out of its own copy. So the next responsive fix would be made in
	// one file and missed in five, which is spec 010's opening sentence again.
	// A subset is now a prop; a *different* rendering is still that site's own.
	//
	// The id is taken separately from the trace because a queue's item names
	// the trace it points at before the trace itself has landed, and that panel
	// shows the id from the row (spec 024 #11).

	let {
		trace = null,
		id = null,
		hide = []
	}: {
		trace?: Trace | null;
		/** The trace id, when it is not the loaded trace's own. */
		id?: string | null;
		/** The parts this site does not render today, and still does not. */
		hide?: ('timestamp' | 'release' | 'session' | 'latency' | 'cost' | 'id' | 'copy')[];
	} = $props();

	const traceID = $derived(id ?? trace?.id ?? null);
	const shown = (part: string) => !hide.includes(part as (typeof hide)[number]);
</script>

{#if trace}
	<!-- The short parts do not shrink; the two ids, which carry `truncate`, are
	     what gives way when the row runs out of room. A sixth part made a flex
	     row of six shrinkable items, and the first thing it cost was a
	     timestamp broken over two lines inside a 48 px bar (spec 023 #17). -->
	{#if shown('timestamp')}
		<span class="hidden shrink-0 font-mono sm:inline">{timestamp(trace.timestamp)}</span>
	{/if}
	<!-- The release, when the trace named one: it is a filter rather than a
	     column, so a panel is where a reader finds out which deployment they
	     are looking at (spec 012, Application contract). -->
	{#if shown('release') && trace.release}
		<span class="hidden truncate md:inline" title="Release">{trace.release}</span>
	{/if}
	<!-- Which session it belongs to, as a link to it (spec 023 #17): the panel
	     is the path spec 008 #3 calls the normal one, and the destination the
	     trace table and the full page offer cannot stop at its door. No
	     `stopPropagation` — a panel's meta has no row click over it. Hidden
	     where the reader is already in that session. -->
	{#if shown('session') && trace.session_id}
		<a
			href="/sessions/{encodeURIComponent(trace.session_id)}"
			title="Everything in {trace.session_id}"
			class="hover:text-fg hidden truncate font-mono hover:underline md:inline"
		>
			{trace.session_id}
		</a>
	{/if}
	{#if shown('latency')}
		<span class="hidden shrink-0 tabular-nums md:inline">{duration(trace.latency_ms)}</span>
	{/if}
	{#if shown('cost')}
		<span class="hidden shrink-0 tabular-nums md:inline">{cost(trace.total_cost)}</span>
	{/if}
{/if}
{#if traceID}
	{#if shown('id')}
		<span class="hidden truncate font-mono lg:inline">{traceID}</span>
	{/if}
	{#if shown('copy')}
		<CopyButton text={traceID} label="Copy the trace id" />
	{/if}
{/if}
