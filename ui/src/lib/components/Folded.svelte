<script lang="ts">
	import { folded } from '$lib/phone';

	// One folded line of a narrow listing's row (spec 006 #18, #22). It wraps
	// rather than truncates — a value cut to an ellipsis is as hidden as one
	// past the edge of a box — and it wraps only between values, after the
	// dot, so a value is never split across two lines and no line starts with
	// a dot. A value longer than the whole line is the one thing cut, and only
	// itself.

	let { values }: { values: readonly (string | null | undefined)[] } = $props();

	const pieces = $derived(folded(values));
</script>

{#each pieces as piece, i (i)}<span class="inline-block max-w-full truncate align-bottom" title={piece}
		>{piece}{i < pieces.length - 1 ? ' ·' : ''}</span
	>{' '}{/each}
