<script lang="ts">
	import type { Run } from '$lib/api/client.svelte';
	import { age } from '$lib/evals';

	// A run's status as the harness said it (spec 014 #8), and beside
	// `running` how long it has been open — the store never guesses completion,
	// so the age is the one number that helps a reader decide whether the
	// harness still will (spec 016 #7). Colour is never the message on its own.

	let {
		status,
		since,
		now = Date.now()
	}: {
		status: Run['status'];
		/** `created_at`, for the age beside `running`. */
		since?: string | null;
		/** The instant to measure from, so a polling page can move it. */
		now?: number;
	} = $props();

	const tone = $derived(
		status === 'running'
			? 'text-warn'
			: status === 'failed'
				? 'text-danger bg-danger-soft'
				: 'text-ok'
	);
</script>

<span class={['inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-xs font-medium', tone]}>
	{status}
	{#if status === 'running' && since}
		<span class="text-subtle font-normal tabular-nums">· {age(since, now)}</span>
	{/if}
</span>
