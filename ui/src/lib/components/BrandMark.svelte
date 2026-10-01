<script lang="ts">
	// The product's mark without its tile, beside the name (spec 006 #30). The
	// geometry is the logo's to the unit — `docs/assets/mark-on-*.svg` — and
	// the rows take the theme's ink through a token, so one drawing is both
	// files and the theme switches it with no second request and no flash.
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
		<path d="M136 168v68a28 28 0 0 0 28 28h32" />
		<path d="M136 236v84a28 28 0 0 0 28 28h32" />
	</g>
	<g class="stroke-brand-ink">
		<path d="M200 168h180" />
		<path d="M252 264h128" />
		<path d="M252 348h128" />
	</g>
</svg>
