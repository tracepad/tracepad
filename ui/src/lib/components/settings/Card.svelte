<script lang="ts">
	import type { Snippet } from 'svelte';

	// One shape for every Settings section, so the screen reads as a list of
	// things you can change rather than as a pile of unrelated forms.

	let {
		title,
		description,
		children
	}: { title: string; description?: string; children: Snippet } = $props();
</script>

<section class="border-border bg-surface rounded-lg border p-4">
	<h2 class="font-medium">{title}</h2>
	{#if description}
		<p class="text-muted mt-1 max-w-prose text-sm">{description}</p>
	{/if}
	<div class="mt-3">{@render children()}</div>
</section>
