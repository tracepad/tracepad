<script lang="ts" module>
	import type { Truncation } from '$lib/api/client.svelte';

	/**
	 * A payload the response could not carry whole (spec 004 #2). The viewer
	 * consumes the marker; it never re-implements the budget that produced it.
	 */
	export function isTruncated(value: unknown): value is Truncation {
		return (
			typeof value === 'object' &&
			value !== null &&
			(value as Truncation).truncated === true &&
			typeof (value as Truncation).full === 'string'
		);
	}
</script>

<script lang="ts">
	import Download from '@lucide/svelte/icons/download';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Button from './Button.svelte';
	import CopyButton from './CopyButton.svelte';
	import JsonNode from './JsonNode.svelte';
	import { ABSENT, bytes } from '$lib/format';

	let {
		label,
		value,
		refused,
		loaded,
		loading,
		onload
	}: {
		label: string;
		value: unknown;
		/** The whole trace refused expansion, so nothing was inlined at all. */
		refused?: boolean;
		/** The payloads have since been fetched, whatever they turned out to be. */
		loaded?: boolean;
		loading: boolean;
		onload: () => void;
	} = $props();

	const marker = $derived(isTruncated(value) ? value : null);
	const present = $derived(value !== undefined && value !== null);
	// `refused` is a fact about the trace and never changes, so "was it
	// refused" cannot answer "is there still something to fetch". Without
	// `loaded`, an observation that genuinely has no metadata keeps offering
	// to load it, forever, and the click changes nothing.
	const pending = $derived(Boolean(refused) && !present && !loaded);
</script>

<section class="border-border border-t px-4 py-3">
	<div class="mb-2 flex items-center gap-1.5">
		<h3 class="text-muted text-xs font-medium tracking-wide uppercase">{label}</h3>
		{#if present && !marker}
			<CopyButton text={() => JSON.stringify(value, null, 2)} label="Copy the whole {label}" />
		{/if}
	</div>

	{#if marker}
		<!-- The preview is a prefix of the payload's JSON cut on a UTF-8
		     boundary, so it is text rather than a value to parse. -->
		{#if marker.preview}
			<pre
				class="text-muted border-border bg-surface max-h-40 overflow-auto rounded-md border p-2
					font-mono text-xs break-all whitespace-pre-wrap">{marker.preview}…</pre>
		{/if}
		<Button class="mt-2" onclick={onload} busy={loading}>
			{#if loading}
				<LoaderCircle class="size-4 animate-spin" />
			{:else}
				<Download class="size-4" />
			{/if}
			Load the whole {bytes(marker.size)}
		</Button>
	{:else if pending}
		<p class="text-muted">
			This trace has more payloads than the response budget can carry markers for, so none were
			inlined. Load this observation's payloads on their own instead.
		</p>
		<Button class="mt-2" onclick={onload} busy={loading}>
			{#if loading}
				<LoaderCircle class="size-4 animate-spin" />
			{:else}
				<Download class="size-4" />
			{/if}
			Load {label.toLowerCase()}
		</Button>
	{:else if present}
		<div class="overflow-x-auto font-mono text-xs">
			<JsonNode {value} />
		</div>
	{:else}
		<p class="text-subtle">{ABSENT}</p>
	{/if}
</section>
