<script lang="ts">
	import { BLOCK_ICONS } from '$lib/dashboard-icons';
	import type { BlockId } from '$lib/dashboard';

	// The small muted glyph before a block's label (spec 034 #16). Decorative:
	// the label beside it is the text. `alert` is Errors with something to
	// show, the one place the glyph takes the danger colour.

	let { id, alert = false }: { id: keyof typeof BLOCK_ICONS & BlockId; alert?: boolean } = $props();
	const Icon = $derived(BLOCK_ICONS[id]);
</script>

<span aria-hidden="true" class="{alert ? 'text-danger' : 'text-muted'} inline-flex shrink-0">
	<Icon class="size-3.5" />
</span>
