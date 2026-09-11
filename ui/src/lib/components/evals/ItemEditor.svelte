<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { untrack } from 'svelte';
	import { ApiError, api, type Dataset } from '$lib/api/client.svelte';
	import { itemBody, savedMessage, short } from '$lib/evals';
	import { href } from '$lib/project.svelte';
	import Button from '../Button.svelte';
	import JsonEditor from '../json/JsonEditor.svelte';
	import PageHeader from '../PageHeader.svelte';

	// The item editor as a full page (spec 016 #5): three documents — the case,
	// what it should answer, and whatever the author wants to remember about it
	// — over spec 015's writing surface. A peek panel is a reading surface half
	// a screen wide, and three editors need the width; a page also has an
	// address, which is what *Add to dataset* links to (#8).
	//
	// Save posts one item with its id, because a re-post of the same id is an
	// edit (spec 014 #9), and shows the store's own answer: a version, or
	// "unchanged" when the write changed nothing. The gate is the editors'
	// `valid` — the linter's answer, which settles shortly after a keystroke
	// (spec 015 #15) — and the body is parsed again on submit, which is what
	// that decision asks a consumer to do.

	let {
		/** The dataset written to; empty until one is chosen (#21). */
		dataset,
		/** The item being edited, when this is an edit of one. */
		id = null,
		/** The observation a case is being cut from (#8). */
		trace = null,
		obs = null,
		/** Called when the select picks another dataset; the route owns the URL. */
		onpick
	}: {
		dataset: string;
		id?: string | null;
		trace?: string | null;
		obs?: string | null;
		onpick?: (name: string) => void;
	} = $props();

	let input = $state('');
	let expected = $state('');
	let metadata = $state('');
	let inputValid = $state(true);
	let expectedValid = $state(true);
	let metadataValid = $state(true);
	/** Where the case came from: a note, not a reference (spec 014 #4). */
	let source = $state.raw<{ trace: string | null; obs: string | null }>({ trace: null, obs: null });
	/**
	 * The id the next Save is an edit of. It is the route's when editing, and
	 * the store's answer after a create — so a second Save edits the item the
	 * first one made rather than adding a second copy of it.
	 */
	let itemID = $state<string | null>(null);
	let loading = $state(false);
	let failure = $state<string | null>(null);
	let busy = $state(false);
	let saved = $state.raw<{ message: string; version: number; id: string } | null>(null);

	$effect(() => {
		// What the panes are seeded from is *what is being edited* — an item, or
		// an observation — and never where the case will be filed. Depending on
		// `dataset` here meant that picking one from the select threw away
		// whatever had been typed, including the correction *Add to dataset*
		// exists to make (found in review of this PR). An item's dataset is in
		// its path and does move it to another item, so it is tracked there.
		const controller = new AbortController();
		const name = id ? dataset : untrack(() => dataset);
		void prefill(name, id, trace, obs, controller.signal);
		return () => controller.abort();
	});

	/**
	 * What the three panes start as. An existing item is read from its
	 * endpoint; a case being cut from an observation is read from
	 * `/observations/{id}/io`, which is the one budget-exempt endpoint — a
	 * preview is a cut document, and a cut document saved as a test case is a
	 * wrong test case (#8).
	 */
	async function prefill(
		name: string,
		wanted: string | null,
		traceID: string | null,
		obsID: string | null,
		signal: AbortSignal
	) {
		failure = null;
		saved = null;
		itemID = wanted;
		source = { trace: traceID, obs: obsID };
		input = expected = metadata = '';
		if (!wanted && !(traceID && obsID)) return;
		loading = true;
		try {
			if (wanted) {
				const item = await api.getItem(name, wanted, undefined, signal);
				if (signal.aborted) return;
				input = document(item.input);
				expected = document(item.expected_output);
				metadata = document(item.metadata);
				source = { trace: item.source_trace_id, obs: item.source_observation_id };
			} else if (traceID && obsID) {
				const io = await api.getObservationIO(obsID, traceID, signal);
				if (signal.aborted) return;
				// The golden case is what the model *should* have said, which is
				// usually its output with a correction — so the output lands in
				// *Expected output* and the author edits it before saving (#8).
				input = document(io.input);
				expected = document(io.output);
			}
		} catch (cause) {
			if (signal.aborted) return;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read what to edit.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	/** A value as the document that holds it; absence is an empty pane. */
	function document(value: unknown): string {
		return value === null || value === undefined ? '' : JSON.stringify(value, null, 2);
	}

	// The datasets to choose from, for a case that does not know where it goes
	// yet. Only when the path has no dataset of its own: an item being edited
	// belongs where it is, and moving it is not an edit (spec 014 has no such
	// write).
	const CHOICES = 250;
	let datasets = $state.raw<Dataset[]>([]);
	/** Whether there are datasets this select is not offering (found in review). */
	let more = $state(false);
	$effect(() => {
		if (id) return;
		const controller = new AbortController();
		api
			.listDatasets({ limit: CHOICES }, controller.signal)
			.then((answer) => {
				if (controller.signal.aborted) return;
				datasets = answer.datasets;
				more = answer.next_cursor !== null;
			})
			.catch(() => {
				// The select stays empty and Save stays shut; the panes are
				// unaffected and nothing was typed twice.
			});
		return () => controller.abort();
	});

	/**
	 * Whether the name in the URL is one the project actually has. A `?dataset=`
	 * that names nothing — a stale link, a typo — must not be savable: the items
	 * endpoint brings a dataset into being on the first write to its name
	 * (spec 014), so a misspelling would quietly create a misspelled dataset
	 * (found in review of this PR). An item being edited is not chosen from a
	 * list at all, so this is asked only where the select is.
	 */
	const known = $derived(id !== null || datasets.some((row) => row.name === dataset));
	const unknown = $derived(id === null && dataset !== '' && !known && datasets.length > 0);

	/**
	 * Whether the panes take a keystroke. Not while the case they are about to
	 * hold is still being read: the prefill assigns all three documents when it
	 * lands, so anything typed into them before that is thrown away without a
	 * word (found alongside the review of this PR).
	 */
	const shut = $derived(busy || loading);

	const ready = $derived(
		dataset !== '' &&
			known &&
			input.trim() !== '' &&
			inputValid &&
			expectedValid &&
			metadataValid &&
			!loading
	);

	async function save() {
		const built = itemBody({
			id: itemID ?? undefined,
			input,
			expected,
			metadata,
			sourceTraceID: source.trace,
			sourceObservationID: source.obs
		});
		if ('problem' in built) {
			failure = built.problem;
			return;
		}
		busy = true;
		failure = null;
		saved = null;
		try {
			const answer = await api.putItem(dataset, built.item);
			itemID = answer.ids[0] ?? itemID;
			saved = {
				message: savedMessage(answer),
				version: answer.version,
				id: itemID ?? ''
			};
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to save the item.';
		} finally {
			busy = false;
		}
	}

	const back = $derived(
		href(dataset === '' ? '/datasets' : `/datasets/${encodeURIComponent(dataset)}`)
	);
	const fieldClass = 'border-border bg-canvas text-fg rounded-md border px-2 py-1 text-sm';
</script>

<svelte:head><title>{id ? 'Edit item' : 'New item'} · Tracepad</title></svelte:head>

<PageHeader title={id ? `Item ${short(id)}` : 'New item'}>
	{#snippet meta()}
		<a href={back} class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			{dataset === '' ? 'Datasets' : dataset}
		</a>
		{#if loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button variant="primary" disabled={!ready} {busy} onclick={save}>
			{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
			Save
		</Button>
	{/snippet}
</PageHeader>

<div class="min-h-0 flex-1 overflow-auto p-4">
	<div class="mx-auto flex max-w-3xl flex-col gap-4">
		{#if !id}
			<!-- Which dataset this case joins. The item editor is where the
			     choice is made, because *Add to dataset* arrives from a trace
			     with no dataset in mind (#8). -->
			<label class="flex flex-col gap-1">
				<span class="text-muted text-xs font-medium">Dataset</span>
				<select
					id="item-dataset"
					name="dataset"
					class="{fieldClass} max-w-sm"
					value={known ? dataset : ''}
					onchange={(event) => onpick?.(event.currentTarget.value)}
				>
					<option value="">Choose a dataset…</option>
					{#each datasets as row (row.name)}
						<option value={row.name}>{row.name}</option>
					{/each}
				</select>
				{#if unknown}
					<span class="text-warn text-xs">
						This project has no dataset called <code class="font-mono">{dataset}</code>. Choose one
						above, or <a class="text-accent underline underline-offset-2" href={href('/datasets')}>make it</a>
						first — saving here would not.
					</span>
				{:else if datasets.length === 0 && dataset === ''}
					<span class="text-subtle text-xs">
						This project has no dataset yet — <a class="text-accent underline underline-offset-2" href={href('/datasets')}>
							make one
						</a> and come back; nothing here is lost by opening it in another tab.
					</span>
				{:else if more}
					<!-- One page of names, and the reader is told so rather than
					     left to conclude their dataset is gone (found in review). -->
					<span class="text-subtle text-xs">
						The first {CHOICES} datasets by name. If this case belongs to another, open it from
						<a class="text-accent underline underline-offset-2" href={href('/datasets')}>Datasets</a>
						and use <em>New item</em>.
					</span>
				{/if}
			</label>
		{/if}

		{#if source.trace}
			<p class="text-subtle truncate font-mono text-xs">
				Cut from
				<a
					class="text-accent underline underline-offset-2"
					href={href(
						`/traces/${encodeURIComponent(source.trace)}${
							source.obs ? `?obs=${encodeURIComponent(source.obs)}` : ''
						}`
					)}
				>
					{source.trace}{source.obs ? ` · ${source.obs}` : ''}
				</a>
			</p>
		{/if}

		{#if failure}
			<p role="alert" class="text-danger flex items-start gap-2 text-sm">
				<TriangleAlert class="mt-0.5 size-4 shrink-0" />
				{failure}
			</p>
		{/if}

		{#if saved}
			<!-- The store's own answer, verbatim in meaning: a write that changed
			     nothing wrote nothing, and only the server can say so. -->
			<p role="status" class="text-ok text-sm">
				{saved.message}
				{#if dataset && saved.id}
					<a
						class="text-accent ml-1 underline underline-offset-2"
						href={href(
							`/datasets/${encodeURIComponent(dataset)}?version=${saved.version}&peek=${saved.id}`
						)}
					>
						Open it in the dataset
					</a>
				{/if}
			</p>
		{/if}

		{#snippet heading(label: string, hint: string)}
			<div class="flex items-baseline gap-2">
				<h2 class="text-muted text-xs font-medium tracking-wide uppercase">{label}</h2>
				<span class="text-subtle text-xs">{hint}</span>
			</div>
		{/snippet}

		<!-- A minimum height on the three surfaces, which grow with their
		     document: an empty editor is otherwise a 24-pixel strip, and a
		     pane somebody is about to write a test case into should look like
		     somewhere to write one. -->
		<section class="flex flex-col gap-1.5 [&_.cm-editor]:min-h-28">
			{@render heading('Input', 'What the case is. Required.')}
			<JsonEditor bind:text={input} bind:valid={inputValid} label="Input" disabled={shut} />
		</section>
		<section class="flex flex-col gap-1.5 [&_.cm-editor]:min-h-28">
			{@render heading('Expected output', 'What a good answer looks like. Optional.')}
			<JsonEditor
				bind:text={expected}
				bind:valid={expectedValid}
				label="Expected output"
				disabled={shut}
				optional
			/>
		</section>
		<section class="flex flex-col gap-1.5 [&_.cm-editor]:min-h-20">
			{@render heading('Metadata', 'Anything the harness or a reader should know. Optional.')}
			<JsonEditor
				bind:text={metadata}
				bind:valid={metadataValid}
				label="Metadata"
				disabled={shut}
				optional
			/>
		</section>

		<p class="text-subtle text-xs">
			Saving writes one item and ticks the dataset's version — unless nothing changed, which
			writes nothing. Every earlier version keeps the row it had.
		</p>
	</div>
</div>
