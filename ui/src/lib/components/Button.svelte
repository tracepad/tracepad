<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLButtonAttributes } from 'svelte/elements';

	type Props = HTMLButtonAttributes & {
		variant?: 'primary' | 'default' | 'ghost';
		/** Marks the control as busy: it stops accepting clicks and says so. */
		busy?: boolean;
		children: Snippet;
	};

	let {
		variant = 'default',
		busy = false,
		disabled = false,
		type = 'button',
		class: extra,
		children,
		...rest
	}: Props = $props();

	// Disabled is both visual and semantic: a control that looks dead but
	// still fires is worse than either.
	const inert = $derived(disabled || busy);
</script>

<button
	{type}
	disabled={inert}
	aria-busy={busy || undefined}
	class={[
		'inline-flex h-7 cursor-pointer items-center gap-1.5 rounded-md border px-2.5 text-sm',
		'font-medium whitespace-nowrap transition-colors duration-100',
		// A finger needs more than a cursor does, and a narrow window on a
		// laptop is still a cursor (spec 006 #15).
		'pointer-coarse:h-11 pointer-coarse:px-4',
		'disabled:cursor-default disabled:opacity-45',
		variant === 'primary' && 'border-accent bg-accent text-on-accent hover:not-disabled:opacity-90',
		variant === 'default' && 'border-border bg-surface text-fg hover:not-disabled:bg-raised',
		variant === 'ghost' && 'border-transparent text-muted hover:not-disabled:bg-raised hover:not-disabled:text-fg',
		extra
	]}
	{...rest}
>
	{@render children()}
</button>
