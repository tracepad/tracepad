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
	import JsonView from './json/JsonView.svelte';
	import MediaStrip from './MediaStrip.svelte';
	import { ABSENT, bytes } from '$lib/format';
	import { mediaRefs } from '$lib/media';

	let {
		label,
		value,
		size,
		refused,
		loaded,
		loading,
		onload
	}: {
		label: string;
		value: unknown;
		/**
		 * The payload's size as the API reports it, shown beside the heading
		 * (spec 012 #6). It is there whether or not the payload itself was
		 * inlined, which is the point: "12 KB in" is what somebody decides
		 * with before asking for the rest.
		 */
		size?: number | null;
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
	/** The images and files the payload points at (spec 041 #10). */
	const media = $derived(present && !marker ? mediaRefs(value) : []);
	/**
	 * What the cut left out (spec 004 #39): references past the preview, which
	 * the server names in the marker because the preview cannot show them. An
	 * image that is in the payload but not on the screen reads as lost.
	 */
	const hidden = $derived(marker?.media ?? []);
	const hiddenCount = $derived(marker?.media_count ?? 0);

	/**
	 * How much of the payload the preview actually is. The marker reports the
	 * whole size in bytes and the server cut the prefix to fit a byte share,
	 * so the two numbers beside each other are counted the same way.
	 */
	const previewBytes = $derived(
		marker?.preview ? new TextEncoder().encode(marker.preview).length : 0
	);
</script>

{#snippet spinner()}
	{#if loading}
		<LoaderCircle class="size-4 shrink-0 animate-spin" />
	{:else}
		<Download class="size-4 shrink-0" />
	{/if}
{/snippet}

<!--
	The truncation banner (spec 015 #3). The whole strip is the button, because
	the sentence *is* the choice spec 004 #2 leaves to the consumer: how much of
	the payload is on screen, how much of it there is, and the one click that
	spends the budget on the rest. Pressing it again after a failure is the
	retry — the owner reports the failure and nothing here was thrown away.
-->
{#snippet markerBanner()}
	{#if marker}
		<button
			type="button"
			onclick={onload}
			disabled={loading}
			aria-busy={loading || undefined}
			class="border-border bg-raised text-muted hover:not-disabled:bg-surface hover:not-disabled:text-fg
				pointer-coarse:min-h-11 flex w-full cursor-pointer items-center gap-1.5 rounded-md border px-2
				py-1.5 text-left transition-colors duration-100 disabled:cursor-default disabled:opacity-60"
		>
			{@render spinner()}
			{#if previewBytes > 0}
				<span class="tabular-nums">showing {bytes(previewBytes)} of {bytes(marker.size)}</span>
				<span class="text-subtle" aria-hidden="true">·</span>
				<span class="text-accent">Load the whole payload</span>
			{:else}
				<!-- Nothing fit under the share, so there is no document to
				     put a proportion on (spec 004 #25). -->
				<span class="text-accent">Load the whole {bytes(marker.size)}</span>
			{/if}
		</button>
	{/if}
{/snippet}

<section class="border-border border-t px-4 py-3">
	<div class="mb-2 flex items-center gap-1.5">
		<h3 class="text-muted text-xs font-medium tracking-wide uppercase">{label}</h3>
		{#if size != null}
			<span class="text-subtle text-xs tabular-nums">{bytes(size)}</span>
		{/if}
	</div>

	{#if marker}
		<!-- The preview is a prefix of the payload's JSON cut on a UTF-8
		     boundary, so it is text and not a value to parse — which is
		     exactly why the surface is a document and not a tree (#3). It is
		     `whole={false}` for the same reason: the banner is how the rest of
		     it is got, and a Copy here would put a prefix on the clipboard. -->
		{#if hiddenCount > 0}
			{#if hidden.length > 0}
				<MediaStrip refs={hidden} />
			{/if}
			<p class="text-muted mb-2 text-xs" data-testid="hidden-media">
				{hiddenCount === 1 ? '1 image or file is' : `${hiddenCount} images or files are`} referenced
				past this preview{hidden.length < hiddenCount
					? ` — ${hidden.length === 0 ? 'none' : 'the first ' + hidden.length} shown here`
					: ''}; the whole payload has {hiddenCount === 1 ? 'it' : 'them'}.
			</p>
		{/if}
		{#if marker.preview}
			<JsonView
				value={marker.preview}
				label="{label} preview"
				banner={markerBanner}
				whole={false}
			/>
		{:else}
			{@render markerBanner()}
		{/if}
	{:else if pending}
		<p class="text-muted">
			This trace has more payloads than the response budget can carry markers for, so none were
			inlined. Load this observation's payloads on their own instead.
		</p>
		<Button class="mt-2" onclick={onload} busy={loading}>
			{@render spinner()}
			Load {label.toLowerCase()}
		</Button>
	{:else if present}
		{#if media.length > 0}
			<MediaStrip refs={media} />
		{/if}
		<JsonView {value} {label} />
	{:else}
		<p class="text-subtle">{ABSENT}</p>
	{/if}
</section>
