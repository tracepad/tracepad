<script lang="ts">
	import Eye from '@lucide/svelte/icons/eye';
	import EyeOff from '@lucide/svelte/icons/eye-off';

	// One labelled field, with the eye a password field needs. A person typing
	// a password they cannot see into a form that will refuse it for a rule
	// they cannot check is the reason the toggle exists.

	let {
		label,
		type = 'text',
		value = $bindable(''),
		autocomplete,
		placeholder,
		hint,
		maxlength
	}: {
		label: string;
		type?: 'text' | 'email' | 'password';
		value?: string;
		autocomplete?: string;
		placeholder?: string;
		hint?: string;
		maxlength?: number;
	} = $props();

	const id = $props.id();
	let reveal = $state(false);
	// `bind:value` needs a static type, so the input takes its value and gives
	// it back by hand — which is what lets one component be all three fields.
	const kind = $derived(type === 'password' && reveal ? 'text' : type);
</script>

<label for={id} class="mt-4 mb-1.5 block font-medium">{label}</label>
<div class="flex gap-1.5">
	<input
		{id}
		type={kind}
		{value}
		oninput={(event) => (value = event.currentTarget.value)}
		autocomplete={autocomplete as never}
		{placeholder}
		{maxlength}
		spellcheck="false"
		autocapitalize="off"
		aria-describedby={hint ? `${id}-hint` : undefined}
		class="border-border bg-canvas placeholder:text-subtle min-w-0 flex-1 rounded-md border px-2.5
			py-1.5 text-sm"
	/>
	{#if type === 'password'}
		<button
			type="button"
			onclick={() => (reveal = !reveal)}
			aria-label={reveal ? 'Hide the password' : 'Show the password'}
			class="border-border bg-surface text-muted hover:bg-raised hover:text-fg
				pointer-coarse:size-11 inline-flex size-8 shrink-0 cursor-pointer items-center
				justify-center rounded-md border transition-colors duration-100"
		>
			{#if reveal}<EyeOff class="size-4" />{:else}<Eye class="size-4" />{/if}
		</button>
	{/if}
</div>
{#if hint}
	<p id="{id}-hint" class="text-subtle mt-1 text-xs">{hint}</p>
{/if}
