<script lang="ts">
	import Button from '../Button.svelte';
	import CodeArea from './CodeArea.svelte';
	import { format } from './setup';

	// The same surface with `readonly` off (spec 015 #2). It takes text rather
	// than a value because an invalid document has to stay on screen while its
	// author fixes it, and no value can hold a syntax error.
	//
	// Nothing is saved here: the component reports whether the text parses and
	// the consumer owns the write (#8). Its first consumer is spec 016.

	let {
		text = $bindable(''),
		label,
		valid = $bindable(true),
		disabled = false
	}: {
		text?: string;
		label: string;
		/** Out: whether the text parses. */
		valid?: boolean;
		disabled?: boolean;
	} = $props();

	/**
	 * The one transform an author asks for (#8). A document that does not parse
	 * is left exactly as it is — the lint marker already says where it stops
	 * being JSON, and rewriting it under the cursor would lose the fix in
	 * progress.
	 */
	function pretty() {
		const formatted = format(text);
		if (formatted !== null) text = formatted;
	}
</script>

<CodeArea bind:text bind:valid {label} {disabled}>
	{#snippet actions()}
		<Button variant="ghost" onclick={pretty} {disabled}>Format</Button>
	{/snippet}
</CodeArea>
