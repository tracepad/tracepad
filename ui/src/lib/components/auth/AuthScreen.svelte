<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import type { Snippet } from 'svelte';
	import Button from '../Button.svelte';

	// The frame the three screens outside the shell share (spec 028 #13):
	// signing in, setting up the first owner, and accepting an invitation. They
	// ask for different things and all three ask the same way — one column, the
	// server's own words under the fields, one button.

	let {
		title,
		intro,
		submitLabel,
		busy = false,
		error = null,
		disabled = false,
		onsubmit,
		children,
		footer
	}: {
		title: string;
		intro: string;
		submitLabel: string;
		busy?: boolean;
		/** What went wrong, in the server's words wherever there are any. */
		error?: string | null;
		disabled?: boolean;
		onsubmit: () => void;
		children: Snippet;
		footer?: Snippet;
	} = $props();
</script>

<svelte:head><title>{title} · Tracepad</title></svelte:head>

<div class="flex min-h-dvh items-center justify-center p-6">
	<form
		class="w-full max-w-sm"
		onsubmit={(event) => {
			event.preventDefault();
			onsubmit();
		}}
	>
		<h1 class="text-xl font-semibold tracking-tight">Tracepad</h1>
		<p class="text-muted mt-1">{intro}</p>

		{@render children()}

		{#if error}
			<p role="alert" class="text-danger mt-3 text-sm">{error}</p>
		{/if}

		<Button type="submit" variant="primary" {busy} {disabled} class="mt-4 w-full justify-center">
			{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
			{submitLabel}
		</Button>

		{#if footer}
			<p class="text-subtle mt-4 text-sm">{@render footer()}</p>
		{/if}
	</form>
</div>
