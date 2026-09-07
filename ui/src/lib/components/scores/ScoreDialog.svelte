<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { Dialog } from 'bits-ui';
	import { ApiError, api, type Score, type ScoreConfig } from '$lib/api/client.svelte';
	import { DATA_TYPES, typeLabel, type DataType } from '$lib/api/score-configs';
	import {
		OTHER,
		configOf,
		emptyScoreForm,
		formOfScore,
		refusedField,
		scoreBody,
		scoreProblem,
		typeOf,
		type ScoreForm,
		type ScoreTarget
	} from '$lib/scores';
	import Button from '../Button.svelte';

	// Scoring by hand (spec 022 #4): one dialog over `POST /api/v1/scores`,
	// which is the endpoint an eval harness posts through and the only write
	// there is. The same component edits (#5) — an edit is that POST carrying
	// the score's own id, because a correction is a re-post (spec 003 #3).
	//
	// The name is a select over the project's configs, and the control under it
	// is the one the config dictates: a range and a category list are typed
	// once, where they are declared. *other…* is the way out for a project that
	// has declared none, and it is the only path that asks for a type.

	let {
		open = false,
		target,
		configs,
		/** The score being corrected, or `null` for a new one. */
		score = null,
		onclose,
		onsaved
	}: {
		open?: boolean;
		target: ScoreTarget;
		configs: ScoreConfig[];
		score?: Score | null;
		onclose: () => void;
		onsaved: () => void;
	} = $props();

	let form = $state<ScoreForm>(emptyScoreForm());
	let busy = $state(false);
	let failure = $state<string | null>(null);

	// Opening seeds the form: one dialog serves every chip, and a value
	// half-typed for one score must not follow the reader to the next.
	$effect(() => {
		if (!open) return;
		form = score ? formOfScore(score, configs) : emptyScoreForm(configs);
		failure = null;
	});

	const config = $derived(configOf(form, configs));
	const type = $derived(typeOf(form, configs));
	const problem = $derived(scoreProblem(form, configs));
	// The form opens on a declared name with only its value missing, so
	// saying "a numeric score needs a value" before anybody typed one is a
	// scold rather than help. The gate speaks once something has been
	// entered — or once a free name is waiting for the type it needs.
	const nag = $derived(
		problem !== null && (form.name.trim() !== '' || form.number !== '' || form.text !== '')
			? problem
			: null
	);
	// The server's refusal belongs at the control that caused it when its
	// sentence names one; otherwise it stays by Save, where it is still read.
	const at = $derived(failure === null ? null : refusedField(failure));

	/**
	 * Picking another name rewrites the value, rather than carrying a number
	 * into a categorical field where the select cannot show it and the server
	 * would refuse it with a sentence about a control nobody touched.
	 */
	function repick(picked: string) {
		form.picked = picked;
		form.dataType = picked === OTHER ? '' : form.dataType;
		form.number = '';
		form.text = '';
	}

	async function save() {
		busy = true;
		failure = null;
		try {
			await api.createScore(scoreBody(target, form, configs, score));
			onsaved();
			onclose();
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to save the score.';
		} finally {
			busy = false;
		}
	}

	const fieldClass = 'border-border bg-canvas text-fg w-full rounded-md border px-2 py-1 text-sm';
	const labelClass = 'mt-3 mb-1 block text-xs font-medium';
</script>

<Dialog.Root {open} onOpenChange={(next) => !next && !busy && onclose()}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="border-border bg-canvas shadow-overlay fixed top-1/2 left-1/2 z-50 max-h-[90dvh]
				w-[min(30rem,calc(100vw-1.5rem))] -translate-x-1/2 -translate-y-1/2 overflow-y-auto
				rounded-lg border p-4"
		>
			<Dialog.Title class="text-lg font-semibold">
				{score ? `Edit ${score.name}` : 'Score'}
			</Dialog.Title>
			<Dialog.Description class="text-muted mt-1 text-sm">
				{#if score}
					A correction is the same score written again, so this replaces the one on screen.
				{:else}
					A judgement about what you are reading. It is one
					<code class="font-mono text-xs">POST /api/v1/scores</code>, the same write an eval
					harness makes.
				{/if}
			</Dialog.Description>

			<label for="score-name" class={labelClass}>Name</label>
			<select
				id="score-name"
				name="picked"
				class={fieldClass}
				value={form.picked}
				onchange={(event) => repick(event.currentTarget.value)}
			>
				{#each configs as one (one.name)}
					<option value={one.name}>{one.name} — {one.data_type}</option>
				{/each}
				<option value={OTHER}>other…</option>
			</select>

			{#if form.picked === OTHER}
				<!-- A project with no configs must still be able to score, and
				     nothing then says what the name means but the person
				     typing it (#4). -->
				<input
					id="score-free-name"
					name="name"
					type="text"
					aria-label="Score name"
					bind:value={form.name}
					placeholder="helpfulness"
					autocomplete="off"
					spellcheck="false"
					class="{fieldClass} mt-1.5 font-mono"
				/>
				<fieldset class="mt-2">
					<legend class="text-xs font-medium">Type</legend>
					<div class="mt-1 flex flex-wrap gap-x-4 gap-y-1">
						{#each DATA_TYPES as value (value)}
							<label class="flex items-center gap-1.5 text-sm">
								<input
									type="radio"
									name="data_type"
									{value}
									checked={form.dataType === value}
									onchange={() => {
										form.dataType = value as DataType;
										form.number = '';
										form.text = '';
									}}
								/>
								{value}
								<span class="sr-only">{typeLabel(value)}</span>
							</label>
						{/each}
					</div>
				</fieldset>
			{:else if config?.description}
				<p class="text-subtle mt-1 text-xs">{config.description}</p>
			{/if}
			{#if at === 'name' && failure}
				<p role="alert" class="text-danger mt-1 text-sm">{failure}</p>
			{/if}

			{#if type !== ''}
				<label for="score-value" class={labelClass}>Value</label>
			{/if}
			{#if type === 'numeric'}
				<!-- Bounded by the config, so the control says what the name
				     admits before the round trip does; the server still
				     decides (#4). -->
				<input
					id="score-value"
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
				<div class="flex gap-1.5" role="group" aria-label="Value">
					{#each [['1', 'yes'], ['0', 'no']] as const as [value, label] (value)}
						<Button
							variant={form.number === value ? 'primary' : 'default'}
							aria-pressed={form.number === value}
							onclick={() => (form.number = value)}
						>
							{label}
						</Button>
					{/each}
				</div>
			{:else if type === 'categorical'}
				<select id="score-value" name="value" class={fieldClass} bind:value={form.text}>
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
					id="score-value"
					name="value"
					rows="3"
					bind:value={form.text}
					placeholder="cites its sources"
					class={fieldClass}
				></textarea>
			{/if}
			{#if at === 'value' && failure}
				<p role="alert" class="text-danger mt-1 text-sm">{failure}</p>
			{/if}

			<label for="score-comment" class={labelClass}>
				Comment <span class="text-subtle font-normal">optional</span>
			</label>
			<input
				id="score-comment"
				name="comment"
				type="text"
				bind:value={form.comment}
				placeholder="why"
				autocomplete="off"
				class={fieldClass}
			/>

			{#if failure && at === null}
				<p role="alert" class="text-danger mt-3 flex items-start gap-2 text-sm">
					<TriangleAlert class="mt-0.5 size-4 shrink-0" />
					{failure}
				</p>
			{:else if nag}
				<p class="text-warn mt-3 text-sm">{nag}</p>
			{/if}

			<div class="mt-4 flex justify-end gap-1.5">
				<Dialog.Close>
					{#snippet child({ props })}
						<Button {...props} disabled={busy}>Cancel</Button>
					{/snippet}
				</Dialog.Close>
				<Button variant="primary" disabled={problem !== null} {busy} onclick={save}>
					{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
					Save
				</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
