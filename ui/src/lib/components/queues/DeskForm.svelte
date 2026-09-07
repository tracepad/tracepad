<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import type { AnnotationItem, Score, ScoreConfig } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import ScoreControl from '$lib/components/scores/ScoreControl.svelte';
	import { changed, deskBody, deskFields, unfilled, type DeskField } from '$lib/queues';
	import { typeOf } from '$lib/scores';

	// The right-hand half of the desk (spec 024 #12): one control per score the
	// queue asks for, in the queue's order, prefilled from what is already on
	// the target — a judge's verdict is a verdict, and the reviewer confirms it
	// rather than repeating it (#7).
	//
	// The control per type is `ScoreControl`, the same one the scoring dialog
	// builds, so a range and a category list are typed once where they are
	// declared. What is this component's own is the *set* of them, the gate
	// that mirrors the server's completeness rule, and what a `409` marks.

	let {
		queue,
		item,
		configs,
		names,
		/** The scores already on this item's target, from the trace's own read. */
		scores,
		annotator,
		/** Names the server refused the last completion for, marked at their control. */
		missing = [],
		busy = false,
		onsave,
		onskip,
		onlater
	}: {
		queue: string;
		item: AnnotationItem;
		configs: ScoreConfig[];
		names: string[];
		scores: Score[];
		annotator: string;
		missing?: string[];
		busy?: boolean;
		/** The bodies to post, in queue order; empty when nothing changed. */
		onsave: (bodies: ReturnType<typeof deskBody>[]) => void;
		onskip: (reason: string) => void;
		onlater: () => void;
	} = $props();

	// A deep `$state`, not `$state.raw`: `ScoreControl` writes into the form
	// it is handed, and a raw array would take the write and tell nobody.
	let fields = $state<DeskField[]>([]);
	let skipping = $state(false);
	let reason = $state('');

	// A new item is a new form. Seeded from the item's id and the scores that
	// came with it, so a value typed for one trace never follows the reviewer
	// to the next one.
	$effect(() => {
		item.id;
		fields = deskFields(names, configs, scores);
		skipping = false;
		reason = '';
	});

	const waiting = $derived(unfilled(fields, configs));
	const target = $derived(
		item.observation_id
			? { trace_id: item.trace_id, observation_id: item.observation_id }
			: { trace_id: item.trace_id }
	);

	function save() {
		const bodies = fields
			.filter((field) => changed(field, configs))
			.map((field) => deskBody(field, configs, target, { queue, annotator }));
		onsave(bodies);
	}

	const fieldClass = 'border-border bg-canvas text-fg w-full rounded-md border px-2 py-1 text-sm';
</script>

<div class="flex min-h-0 flex-1 flex-col overflow-y-auto p-4">
	{#each fields as field, index (field.name)}
		<section class={['border-border', index > 0 && 'mt-4 border-t pt-3']}>
			<div class="flex items-baseline gap-2">
				<h3 class="font-medium">{field.name}</h3>
				{#if field.existing}
					<!-- Prefilled from a verdict already on this target: the
					     reviewer confirms or edits it (#7, #12). -->
					<span class="text-subtle text-xs">already scored</span>
				{/if}
				{#if missing.includes(field.name)}
					<span role="alert" class="text-danger text-xs">the server has no score for this</span>
				{/if}
			</div>
			{#if field.config?.description}
				<p class="text-subtle mt-0.5 text-xs">{field.config.description}</p>
			{:else if !field.config}
				<!-- The config was deleted and the queue kept the name (edge
				     cases): the score is still writable, free-typed, and the
				     note says why nothing bounds it. -->
				<p class="text-warn mt-0.5 text-xs">
					This name has no score config any more, so nothing says what it admits.
				</p>
			{/if}

			<ScoreControl
				type={typeOf(field.form, configs)}
				config={field.config}
				label={field.name}
				bind:form={fields[index].form}
			/>

			<label for="comment-{index}" class="mt-2 mb-1 block text-xs font-medium">
				Comment <span class="text-subtle font-normal">optional</span>
			</label>
			<input
				id="comment-{index}"
				type="text"
				bind:value={fields[index].form.comment}
				placeholder="why"
				autocomplete="off"
				class={fieldClass}
			/>
		</section>
	{/each}

	{#if waiting.length > 0}
		<!-- The client-side half of Decision 7, which mirrors the server's rule
		     rather than replacing it: the `409` is still the oracle, and it
		     marks the controls when the two disagree. -->
		<p class="text-warn mt-4 text-sm">
			Still to set: {waiting.join(', ')}.
		</p>
	{/if}

	{#if skipping}
		<div class="border-border mt-4 rounded-md border p-3">
			<label for="skip-reason" class="mb-1 block text-xs font-medium">Why skip this one?</label>
			<input
				id="skip-reason"
				type="text"
				bind:value={reason}
				placeholder="nothing to judge here"
				autocomplete="off"
				class={fieldClass}
			/>
			<div class="mt-2 flex gap-1.5">
				<Button variant="primary" {busy} onclick={() => onskip(reason)}>Skip it</Button>
				<Button onclick={() => (skipping = false)}>Cancel</Button>
			</div>
		</div>
	{/if}

	<div class="mt-4 flex flex-wrap gap-1.5">
		<Button variant="primary" disabled={waiting.length > 0} {busy} onclick={save}>
			{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
			Complete &amp; next
		</Button>
		{#if !skipping}
			<Button onclick={() => (skipping = true)}>Skip…</Button>
		{/if}
		<!-- *Later* releases the claim so nobody waits out its ten minutes, and
		     leaves the desk (Decision 16). -->
		<Button variant="ghost" {busy} onclick={onlater}>Later</Button>
	</div>

	{#if missing.length > 0}
		<p role="alert" class="text-danger mt-3 flex items-start gap-2 text-sm">
			<TriangleAlert class="mt-0.5 size-4 shrink-0" />
			The server would not complete this item: it found no score for {missing.join(', ')}.
		</p>
	{/if}
</div>
