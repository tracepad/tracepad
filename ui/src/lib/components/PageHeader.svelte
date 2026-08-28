<script lang="ts">
	import type { Snippet } from 'svelte';

	// The one horizontal rule between the shell and a screen. Every page wears
	// the same 48px bar so that switching screens never moves the content down
	// or up a few pixels.
	let { title, meta, actions }: { title: string; meta?: Snippet; actions?: Snippet } = $props();
</script>

<header
	class="border-border flex h-12 shrink-0 items-center gap-3 border-b px-4"
>
	<h1 class="text-lg font-semibold tracking-tight whitespace-nowrap">{title}</h1>
	{#if meta}
		<div class="text-subtle flex min-w-0 items-center gap-2 text-sm">{@render meta()}</div>
	{/if}
	{#if actions}
		<div class="ml-auto flex items-center gap-1.5">{@render actions()}</div>
	{/if}
</header>
