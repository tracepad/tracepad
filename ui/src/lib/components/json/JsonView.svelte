<script lang="ts">
	import type { Snippet } from 'svelte';
	import CodeArea from './CodeArea.svelte';
	import { asDocument } from './setup';

	// A payload, read-only (spec 015 #2). It takes a value because every caller
	// has one — the API's JSON — and turns it into the document itself, so that
	// two screens showing the same payload cannot disagree about indentation,
	// about how long a string may be, or about what a bare string is.
	//
	// A *document* is what this is for: `input`, `output`, `metadata`, a
	// trace's metadata — nested, and as big as the model made it. A flat record
	// of ten scalars, such as an observation's `usage` or `cost_details`, is a
	// two-column list and not a document (#11): nobody searches a usage block,
	// and a mounted CodeMirror is a gutter, a keymap and a `contenteditable`.

	let {
		value,
		label,
		folded,
		whole = true,
		banner
	}: {
		value: unknown;
		label: string;
		folded?: boolean;
		/**
		 * Whether this value is the whole payload. A truncation marker's
		 * preview is a prefix of one, and gets no Copy (#3).
		 */
		whole?: boolean;
		/** Shown above the toolbar; the truncation banner of #3 is one. */
		banner?: Snippet;
	} = $props();

	const shown = $derived(asDocument(value));
</script>

<CodeArea text={shown.text} plain={shown.plain} {label} {folded} {whole} {banner} readonly />
