<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { Dialog } from 'bits-ui';
	import { ApiError, api, type ScoreConfig } from '$lib/api/client.svelte';
	import {
		DATA_TYPES,
		DIRECTIONS,
		bounded,
		configBody,
		configProblem,
		directionLabel,
		emptyForm,
		formOf,
		judged,
		typeLabel,
		type ConfigForm,
		type DataType,
		type Direction
	} from '$lib/api/score-configs';
	import Button from '../Button.svelte';

	// What a score name means, as a form (spec 016 #9). The endpoint is
	// declarative — the whole config, every time (spec 014 #17) — so one form
	// creates and edits, and a re-save of an unchanged config is a no-op on the
	// server rather than a second row.
	//
	// The rules between the fields are spec 014 #16 mirrored here so that the
	// form refuses before the round trip and says the same thing the round trip
	// would; the server stays the oracle, and its refusal is shown verbatim
	// when the two ever disagree.

	let {
		open = false,
		/** The config being edited, or `null` for a new name. */
		config = null,
		onclose,
		onsaved
	}: {
		open?: boolean;
		config?: ScoreConfig | null;
		onclose: () => void;
		onsaved: () => void;
	} = $props();

	let form = $state<ConfigForm>(emptyForm());
	let busy = $state(false);
	let failure = $state<string | null>(null);

	// Opening is what seeds the form: the same dialog serves every row, and a
	// config half-typed for one name must not follow the reader to another.
	$effect(() => {
		if (!open) return;
		form = config ? formOf(config) : emptyForm();
		failure = null;
	});

	/**
	 * A type change rewrites what the type does not take, rather than leaving
	 * it to be refused: switching to `categorical` and being told "a
	 * categorical name has no direction" about a field that is no longer on
	 * screen would be a dead end.
	 */
	function retype(value: DataType) {
		form.data_type = value;
		form.direction = judged(value) ? (form.direction === '' ? 'higher' : form.direction) : '';
		if (!bounded(value)) form.min = form.max = '';
		if (value !== 'categorical') form.categories = '';
	}

	const problem = $derived(configProblem(form));

	async function save() {
		busy = true;
		failure = null;
		try {
			await api.putScoreConfig(form.name.trim(), configBody(form));
			onsaved();
			onclose();
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to save the config.';
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
				{config ? `Score config ${config.name}` : 'New score config'}
			</Dialog.Title>
			<Dialog.Description class="text-muted mt-1 text-sm">
				What this score name means, for every harness that posts it. From here on a score under
				this name must fit — the store refuses the batch that does not.
			</Dialog.Description>

			<label for="config-name" class={labelClass}>Name</label>
			<input
				id="config-name"
				name="name"
				type="text"
				bind:value={form.name}
				readonly={config !== null}
				placeholder="accuracy"
				autocomplete="off"
				spellcheck="false"
				class="{fieldClass} font-mono read-only:opacity-60"
			/>
			{#if config}
				<p class="text-subtle mt-1 text-xs">
					The name is what binds the scores; make another config to bind another name.
				</p>
			{/if}

			<label for="config-type" class={labelClass}>Type</label>
			<select
				id="config-type"
				name="data_type"
				class={fieldClass}
				value={form.data_type}
				onchange={(event) => retype(event.currentTarget.value as DataType)}
			>
				{#each DATA_TYPES as value (value)}
					<option {value}>{typeLabel(value)}</option>
				{/each}
			</select>

			{#if judged(form.data_type)}
				<label for="config-direction" class={labelClass}>Direction</label>
				<select
					id="config-direction"
					name="direction"
					class={fieldClass}
					value={form.direction}
					onchange={(event) => (form.direction = event.currentTarget.value as Direction)}
				>
					{#each DIRECTIONS as value (value)}
						<option {value}>{directionLabel(value)}</option>
					{/each}
				</select>
				<p class="text-subtle mt-1 text-xs">
					This is what lets a comparison say <em>improved</em> rather than <em>changed</em>.
				</p>
			{/if}

			{#if bounded(form.data_type)}
				<!-- The bounds stay text under the number input: an empty bound
				     and a bound of zero are two different declarations, and
				     `bind:value` on a number field turns both into `null`. -->
				<div class="flex gap-3">
					<div class="min-w-0 flex-1">
						<label for="config-min" class={labelClass}>
							Minimum <span class="text-subtle font-normal">optional</span>
						</label>
						<input
							id="config-min"
							name="min"
							type="number"
							step="any"
							value={form.min}
							oninput={(event) => (form.min = event.currentTarget.value)}
							class="{fieldClass} tabular-nums"
						/>
					</div>
					<div class="min-w-0 flex-1">
						<label for="config-max" class={labelClass}>
							Maximum <span class="text-subtle font-normal">optional</span>
						</label>
						<input
							id="config-max"
							name="max"
							type="number"
							step="any"
							value={form.max}
							oninput={(event) => (form.max = event.currentTarget.value)}
							class="{fieldClass} tabular-nums"
						/>
					</div>
				</div>
			{/if}

			{#if form.data_type === 'categorical'}
				<label for="config-categories" class={labelClass}>Categories, one per line</label>
				<textarea
					id="config-categories"
					name="categories"
					rows="4"
					bind:value={form.categories}
					placeholder={'correct\npartial\nwrong'}
					class="{fieldClass} font-mono"
				></textarea>
			{/if}

			<label for="config-description" class={labelClass}>
				Description <span class="text-subtle font-normal">optional</span>
			</label>
			<input
				id="config-description"
				name="description"
				type="text"
				bind:value={form.description}
				placeholder="How close the answer was to the expected one"
				autocomplete="off"
				class={fieldClass}
			/>

			{#if failure}
				<p role="alert" class="text-danger mt-3 flex items-start gap-2 text-sm">
					<TriangleAlert class="mt-0.5 size-4 shrink-0" />
					{failure}
				</p>
			{:else if problem && form.name.trim() !== ''}
				<p class="text-warn mt-3 text-sm">{problem}</p>
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
