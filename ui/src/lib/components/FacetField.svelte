<script lang="ts">
	import { untrack } from 'svelte';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import type { FacetValue } from '$lib/api/client.svelte';
	import { FILTER_BOX_FROM, facetOptions, narrowFacet } from '$lib/api/facets';

	// One many-valued filter as a checkbox list (spec 027 #6): the values the
	// range holds, each with its count, and whatever the URL already carries.
	//
	// A checkbox is how a person says *these* rather than *this*, and the count
	// beside it is what makes the choice informed — `prod: 1` is a typo,
	// `production: 4656` is the environment. There is no free-text box: the
	// list plus the live tail behind it is complete, so a value worth typing is
	// a value already on it.

	let {
		id,
		label,
		name,
		values,
		checked,
		omitted = 0,
		loading = false,
		failure = null,
		onchange
	}: {
		id: string;
		/** The field's own name, for the labels a screen reader reads. */
		label: string;
		/** The filter this list sets, which groups the boxes for the form. */
		name: string;
		/** What `GET /api/v1/facets` returned for this column, in its order. */
		values: FacetValue[];
		/** What the URL carries, which may name a value the range no longer has. */
		checked: string[];
		omitted?: number;
		loading?: boolean;
		failure?: string | null;
		onchange: (next: string[]) => void;
	} = $props();

	// The out-of-list values, remembered rather than derived. Unchecking one
	// must leave its box where it is: derived from `checked`, the row would
	// vanish on the click that cleared it, taking with it the only control
	// that could put it back before Apply.
	let pinned = $state<string[]>([]);
	$effect(() => {
		const known = new Set(values.map((one) => one.value));
		const next = [...new Set([...untrack(() => pinned), ...checked])].filter(
			(value) => !known.has(value)
		);
		if (next.join('\u0000') !== untrack(() => pinned).join('\u0000')) pinned = next;
	});

	// While the values are in flight — or after a failed read — this is the
	// checked ones alone, which is what keeps an existing filter visible and
	// removable throughout (Decision 6).
	const options = $derived(facetOptions(values, checked, pinned));

	let query = $state('');
	const shown = $derived(narrowFacet(options, query));
	// The box appears only when a list is long enough to need one, for the
	// reason the search box is not on every listing.
	const filterable = $derived(options.length > FILTER_BOX_FROM);

	function toggle(value: string, on: boolean) {
		onchange(on ? [...checked, value] : checked.filter((one) => one !== value));
	}

	const boxClass =
		'border-border bg-canvas placeholder:text-subtle w-full rounded-md border px-2 py-1 text-sm';
</script>

{#if failure}
	<p role="alert" class="text-danger flex items-start gap-1 text-xs">
		<TriangleAlert class="mt-0.5 size-3 shrink-0" />
		{failure}
	</p>
{/if}

{#if filterable}
	<!-- The label is visually redundant beside the field's own heading and is
	     not redundant to a screen reader, which meets the box on its own. -->
	<label class="sr-only" for="{id}-filter">Filter the {label} values</label>
	<input
		id="{id}-filter"
		name="{id}-filter"
		type="text"
		bind:value={query}
		placeholder="Filter values"
		autocomplete="off"
		spellcheck="false"
		class="{boxClass} mb-1"
	/>
{/if}

<!-- Ten rows and the list scrolls inside the panel rather than growing it past
     the viewport; at 375 px that is what keeps Apply reachable. -->
<div
	class="border-border max-h-52 overflow-y-auto rounded-md border"
	role="group"
	aria-labelledby="{id}-label"
>
	{#each shown as option, index (option.value)}
		<label
			class="hover:bg-raised pointer-coarse:min-h-11 flex cursor-pointer items-center gap-2 px-2 py-1
				text-sm transition-colors duration-100"
		>
			<!-- The box is named for the value alone, with the count read after
			     it: "production, 4656 traces" is what a screen reader should
			     say, and the two spans read as one string otherwise. -->
			<input
				type="checkbox"
				id="{id}-{index}"
				{name}
				checked={option.checked}
				aria-label={option.count === null
					? option.value
					: `${option.value}, ${option.count} traces`}
				onchange={(event) => toggle(option.value, event.currentTarget.checked)}
				class="accent-accent size-3.5 shrink-0"
			/>
			<span class="min-w-0 flex-1 truncate">{option.value}</span>
			{#if option.count !== null}
				<span class="text-subtle shrink-0 text-xs tabular-nums">{option.count}</span>
			{/if}
		</label>
	{:else}
		<p class="text-subtle px-2 py-1.5 text-xs">
			{#if loading}
				Loading…
			{:else if query}
				Nothing matches “{query}”.
			{:else if failure}
				No values to offer.
			{:else}
				Nothing in this range.
			{/if}
		</p>
	{/each}
</div>

{#if omitted > 0}
	<!-- A capped list that said nothing about it would be a wrong one. -->
	<p class="text-subtle mt-0.5 text-xs">and {omitted} more, too rare to list</p>
{/if}
