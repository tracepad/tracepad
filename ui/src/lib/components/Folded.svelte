<script lang="ts">
	import { folded } from '$lib/phone';

	// One folded line of a narrow listing's row (spec 006 #18, #22). It wraps
	// rather than truncates — a value cut to an ellipsis is as hidden as one
	// past the edge of a box — and it wraps only between values, after the
	// dot, so a value is never split across two lines and no line starts with
	// a dot. A value longer than the whole line is the one thing cut, and only
	// itself.

	// A value may carry a tooltip of its own, for a figure that has more to say
	// than its text (spec 049 #9); it replaces the default, which repeats the text
	// for the sake of a value cut to an ellipsis.
	type Value = string | null | undefined | { text: string | null | undefined; title?: string };

	let { values }: { values: readonly Value[] } = $props();

	const pieces = $derived(
		values
			.map((one) => (one && typeof one === 'object' ? one : { text: one, title: undefined }))
			.filter((one) => folded([one.text]).length > 0)
			.map((one) => ({ text: one.text as string, title: one.title }))
	);
</script>

{#each pieces as piece, i (i)}<span
		class="inline-block max-w-full truncate align-bottom"
		title={piece.title ?? piece.text}>{piece.text}{i < pieces.length - 1 ? ' ·' : ''}</span
	>{' '}{/each}
