<script lang="ts">
	import Check from '@lucide/svelte/icons/check';
	import Copy from '@lucide/svelte/icons/copy';

	// Copying is the one action this read-only interface offers everywhere, so
	// it behaves the same everywhere: the icon becomes a tick for a moment,
	// which is the whole confirmation a copy needs.
	let { text, label = 'Copy' }: { text: string | (() => string); label?: string } = $props();

	let copied = $state(false);
	let timer: ReturnType<typeof setTimeout> | undefined;

	async function copy() {
		try {
			await navigator.clipboard.writeText(typeof text === 'function' ? text() : text);
			copied = true;
			clearTimeout(timer);
			timer = setTimeout(() => (copied = false), 1200);
		} catch {
			// A browser that refuses the clipboard (no permission, no secure
			// context) leaves the text selectable, which is the fallback.
		}
	}
</script>

<button
	type="button"
	onclick={copy}
	title={copied ? 'Copied' : label}
	aria-label={copied ? 'Copied' : label}
	class="text-subtle hover:bg-raised hover:text-fg pointer-coarse:size-11 inline-flex size-6
		shrink-0 cursor-pointer items-center justify-center rounded transition-colors duration-100"
>
	{#if copied}
		<Check class="text-ok size-3.5" />
	{:else}
		<Copy class="size-3.5" />
	{/if}
</button>
