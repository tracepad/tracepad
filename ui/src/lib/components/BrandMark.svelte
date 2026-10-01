<script lang="ts">
	// The product's mark without its tile, beside the name (spec 006 #30): the
	// geometry of `docs/assets/mark-on-*.svg` path for path, which
	// `BrandMark.test.ts` holds it to, and the rows in the theme's ink through
	// a token, so one drawing is both files and the theme switches it with no
	// second request and no flash.
	// Decorative: the name next to it is what a screen reader reads.
	let { class: className = '' }: { class?: string } = $props();
</script>

<svg
	viewBox="96 128 324 260"
	class="shrink-0 {className}"
	fill="none"
	stroke-linecap="round"
	stroke-linejoin="round"
	stroke-width="40"
	aria-hidden="true"
>
	<g class="stroke-brand-tree">
		<path d="M136 168 V236 a28 28 0 0 0 28 28 H196" />
		<path d="M136 236 V320 a28 28 0 0 0 28 28 H196" />
	</g>
	<g class="stroke-brand-ink">
		<path d="M200 168 H380" />
		<path d="M252 264 H380" />
		<path d="M252 348 H380" />
	</g>
</svg>
