<script lang="ts">
	import { LOGIN_ROUTE } from '$lib/auth.svelte';

	// What `/setup` shows once this server has an owner, and what `/invite`
	// shows without a token (spec 028, Application contract): one line saying
	// why there is nothing to fill in, and the way to the sign-in form.

	let { title, children }: { title: string; children: import('svelte').Snippet } = $props();
</script>

<svelte:head><title>{title} · Tracepad</title></svelte:head>

<div class="flex min-h-dvh items-center justify-center p-6">
	<div class="w-full max-w-sm">
		<h1 class="text-xl font-semibold tracking-tight">Tracepad</h1>
		<p class="text-muted mt-1">{@render children()}</p>
		<a href={LOGIN_ROUTE} class="text-accent mt-4 inline-block font-medium hover:underline">
			Go to the sign-in form
		</a>
	</div>
</div>
