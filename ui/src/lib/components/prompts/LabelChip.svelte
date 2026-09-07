<script lang="ts">
	import X from '@lucide/svelte/icons/x';

	// One label, everywhere a label is shown (spec 021). `production` is the
	// one that means "this is live", so it is the one that is coloured; every
	// other label is a note somebody left and reads as one.
	//
	// The × is optional: the listing shows labels, the version view edits them
	// (Decision 6), and both want the same chip.

	let {
		label,
		version,
		onremove,
		/** Whether the × may be pressed, and what to say when it may not. */
		disabled = false,
		reason
	}: {
		label: string;
		version?: number;
		onremove?: () => void;
		disabled?: boolean;
		reason?: string;
	} = $props();

	const live = $derived(label === 'production');
</script>

<span
	class={[
		'inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-medium',
		live ? 'border-accent bg-accent-soft text-accent' : 'border-border text-muted'
	]}
>
	{label}{#if version !== undefined}<span class="tabular-nums opacity-70">v{version}</span>{/if}
	{#if onremove}
		<button
			type="button"
			onclick={onremove}
			{disabled}
			aria-label="Remove {label}"
			title={disabled ? (reason ?? `Remove ${label}`) : `Remove ${label}`}
			class="hover:not-disabled:text-danger -mr-0.5 cursor-pointer rounded-full transition-colors
				duration-100 disabled:cursor-default disabled:opacity-45"
		>
			<X class="size-3" />
		</button>
	{/if}
</span>
