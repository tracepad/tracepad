<script lang="ts">
	import EyeOff from '@lucide/svelte/icons/eye-off';
	import GripVertical from '@lucide/svelte/icons/grip-vertical';
	import type { Snippet } from 'svelte';
	import { dragHandle } from 'svelte-dnd-action';
	import Button from '../Button.svelte';

	// The frame every block wears (spec 034 #8). Outside Customize it is
	// nothing — the block draws itself. In Customize it grows a handle to drag
	// by and a control to hide it, and the handle is also the keyboard path:
	// focus it, space to lift, arrows to move, space to drop, as the library
	// announces.

	let {
		label,
		customizing = false,
		onhide,
		children
	}: {
		label: string;
		customizing?: boolean;
		onhide?: () => void;
		children: Snippet;
	} = $props();
</script>

<div class="relative min-w-0">
	{#if customizing}
		<div
			class="border-accent bg-canvas absolute -top-2.5 right-2 z-10 flex items-center gap-0.5 rounded-md border px-1 py-0.5 shadow-sm"
		>
			<span
				use:dragHandle
				aria-label="Move {label}"
				class="text-muted hover:text-fg inline-flex size-6 cursor-grab items-center justify-center rounded"
			>
				<GripVertical class="size-4" aria-hidden="true" />
			</span>
			<Button variant="ghost" class="h-6 px-1.5 text-xs" onclick={onhide} aria-label="Hide {label}">
				<EyeOff class="size-3.5" aria-hidden="true" />
				Hide
			</Button>
		</div>
	{/if}
	<!-- Inert while customizing: the block is a thing to move, not to read,
	     and the keyboard path runs through the handles alone. -->
	<div inert={customizing || undefined}>
		{@render children()}
	</div>
</div>
