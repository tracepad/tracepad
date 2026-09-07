<script lang="ts">
	import type { ScoreConfig } from '$lib/api/client.svelte';
	import type { ScoreForm } from '$lib/scores';
	import Button from '../Button.svelte';

	// The control a score's type dictates (spec 022 #4), extracted so that the
	// dialog and the annotation desk build it the same way (spec 024 #12).
	//
	// One rule, in one place: the config says what the name means, and the
	// control is derived from that rather than from a second declaration. A
	// range and a category list are typed once, where they are declared — so a
	// reviewer at the desk and a reader on a trace meet the same field.
	//
	// It renders the value and nothing else. Which name is being scored, what
	// the comment says and what Save posts belong to whoever is using it.

	let {
		/** What kind of value to ask for; nothing is drawn until it is known. */
		type,
		/** The config that bounds it, when the name has one. */
		config = undefined,
		/** The value halves, as the inputs carry them (spec 022 #4). */
		form = $bindable(),
		/** What the field is called; the desk names each control by its score. */
		label = 'Value'
	}: {
		type: string;
		config?: ScoreConfig;
		form: ScoreForm;
		label?: string;
	} = $props();

	const id = $props.id();

	const fieldClass = 'border-border bg-canvas text-fg w-full rounded-md border px-2 py-1 text-sm';
	const labelClass = 'mt-3 mb-1 block text-xs font-medium';
</script>

{#if type !== ''}
	<!-- `for` only where there is a control to point at: a boolean's value is
	     two buttons, and a label pointing at an id nothing carries names
	     nothing. That branch borrows this element as its group label instead. -->
	<label id="{id}-label" for={type === 'boolean' ? undefined : id} class={labelClass}>
		{label}
	</label>
{/if}

{#if type === 'numeric'}
	<!-- Bounded by the config, so the control says what the name admits before
	     the round trip does; the server still decides (spec 022 #4). -->
	<input
		{id}
		name="value"
		type="number"
		step="any"
		min={config?.min ?? undefined}
		max={config?.max ?? undefined}
		value={form.number}
		oninput={(event) => (form.number = event.currentTarget.value)}
		class="{fieldClass} tabular-nums"
	/>
	{#if config?.min != null || config?.max != null}
		<p class="text-subtle mt-1 text-xs">
			{config?.min ?? 'anything'} … {config?.max ?? 'anything'}
		</p>
	{/if}
{:else if type === 'boolean'}
	<div class="flex gap-1.5" role="group" aria-labelledby="{id}-label">
		{#each [['1', 'yes'], ['0', 'no']] as const as [value, word] (value)}
			<Button
				variant={form.number === value ? 'primary' : 'default'}
				aria-pressed={form.number === value}
				onclick={() => (form.number = value)}
			>
				{word}
			</Button>
		{/each}
	</div>
{:else if type === 'categorical'}
	<select {id} name="value" class={fieldClass} bind:value={form.text}>
		<option value="" disabled>Pick one…</option>
		{#each config?.categories ?? [] as category (category)}
			<option value={category}>{category}</option>
		{/each}
	</select>
	{#if (config?.categories ?? []).length === 0}
		<p class="text-warn mt-1 text-xs">
			This name is categorical and its config lists no categories.
		</p>
	{/if}
{:else if type === 'text'}
	<textarea
		{id}
		name="value"
		rows="3"
		bind:value={form.text}
		placeholder="cites its sources"
		class={fieldClass}
	></textarea>
{/if}
