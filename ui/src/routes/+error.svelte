<script lang="ts">
	import { page } from '$app/state';
	import { href } from '$lib/project.svelte';
</script>

<div class="flex flex-1 items-center justify-center p-8">
	<div class="max-w-md text-center">
		<p class="text-subtle font-mono text-xs">{page.status}</p>
		<h1 class="mt-1 text-xl font-semibold">
			{page.status === 404 ? 'No such page' : 'Something went wrong'}
		</h1>
		<p class="text-muted mt-2">
			{page.status === 404 ? page.url.pathname : (page.error?.message ?? 'Unknown error')}
		</p>
		<a href={href('/traces')} class="text-accent mt-4 inline-block underline underline-offset-2">
			Back to traces
		</a>
	</div>
</div>
