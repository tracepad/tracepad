<script lang="ts">
	import type { Snippet } from 'svelte';
	import { project } from '$lib/project.svelte';

	// A screen whose whole purpose is to write (spec 028 #15): the prompt
	// editor, the dataset item editor. The listing behind it is a viewer's to
	// read, so the route stays reachable and says what it is rather than
	// rendering a form whose only ending is a 403.

	let { children, what }: { children: Snippet; what: string } = $props();
</script>

{#if project.editor}
	{@render children()}
{:else}
	<div class="min-h-0 flex-1 overflow-auto p-4">
		<p class="text-muted max-w-prose text-sm">
			Your role in this project is viewer, so {what}. Ask an owner for the editor role.
		</p>
	</div>
{/if}
