<script lang="ts">
	import type { AnnotationQueue } from '$lib/api/client.svelte';
	import { count } from '$lib/format';
	import { progress } from '$lib/queues';

	// How far a queue has got (spec 024 #10): completed filled, skipped
	// hatched, the rest waiting. The hatch is what keeps *done* honest — a
	// skipped item is finished with and is not a verdict, and a bar that
	// counted the two the same would say a queue nobody judged is complete.

	let {
		queue,
		wide = false,
		stacked = false
	}: {
		queue: AnnotationQueue;
		wide?: boolean;
		/** The count under the bar, for a folded listing's narrow column (spec 006 #22). */
		stacked?: boolean;
	} = $props();

	const at = $derived(progress(queue));
	const title = $derived(
		`${count(at.completed)} completed, ${count(at.skipped)} skipped, ${count(queue.counts.pending)} to do`
	);
</script>

<div class={['flex', stacked ? 'flex-col items-start gap-1' : 'items-center gap-2']} {title}>
	<div
		class={[
			'bg-raised h-1.5 overflow-hidden rounded-full',
			wide ? 'w-32' : 'w-24 shrink-0'
		]}
		role="progressbar"
		aria-valuenow={at.completed}
		aria-valuemin={0}
		aria-valuemax={at.total}
		aria-label="Completed"
	>
		<div class="flex h-full">
			<div class="bg-accent h-full" style="width: {at.completedPercent}%"></div>
			<!-- Hatched rather than a second colour: a skip is not a second kind
			     of progress, it is progress that produced no verdict. -->
			<div
				class="h-full bg-[repeating-linear-gradient(45deg,var(--color-subtle)_0_2px,transparent_2px_4px)]"
				style="width: {at.skippedPercent}%"
			></div>
		</div>
	</div>
	<span class="text-subtle shrink-0 text-xs tabular-nums">{at.label}</span>
</div>
