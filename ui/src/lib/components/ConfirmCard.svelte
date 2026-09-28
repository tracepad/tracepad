<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import type { Snippet } from 'svelte';
	import { ApiError, type DryRun } from '$lib/api/client.svelte';
	import { count, timestamp } from '$lib/format';
	import Button from './Button.svelte';

	// Every destructive act in this interface, rendered the same way (spec 007
	// #5): ask the server what it would do, show that answer, and enable the
	// button only once the person has typed back the identity the server asked
	// for.
	//
	// Spec 005 #8 built the dry-run/confirm contract so that "CLI and UI
	// confirmation dialogs cost the server nothing extra". This card is that
	// promise cashed in. It computes no preview of its own: a count the browser
	// guessed is a count that can be wrong, and typing the echo here is the
	// same speed bump `curl` faces — the interface must not be the softer path
	// to destruction.

	let {
		title,
		description,
		/** What the echo is called, e.g. "project name" or "user id". */
		echoLabel,
		previewLabel = 'Preview',
		executeLabel,
		/**
		 * Asks the server. It answers with its dry run — or, when what was
		 * asked destroys nothing, with the act already done and a sentence
		 * saying so (a retention window that grew, a key that was not the
		 * last one).
		 */
		preview,
		/** The real thing, with the echo the server named. Returns what to say. */
		execute,
		/**
		 * What the preview is about — the request body, the id being erased,
		 * the row being deleted. A dry run describes one particular change,
		 * but the echo the server asks for names the project, so nothing on
		 * the wire ties the answer to the question. Change this and the plan
		 * on screen is dropped: what gets confirmed is what was shown.
		 */
		subject,
		/** Whether there is enough here to ask the server about yet. */
		ready = true,
		/**
		 * Ask the server as soon as the card is on screen, rather than on a
		 * click: for a dialog that opened *because* somebody chose to delete
		 * this one thing, the preview is what the dialog is (spec 035 #8).
		 */
		immediate = false,
		/**
		 * Fill the echo in from the plan rather than have it typed. Only where
		 * the identity is already on screen and is an id: typing thirty-two
		 * hex characters back is a ritual, not a check, and the server checks
		 * the string either way (spec 035 #8).
		 */
		prefill = false,
		/** Extra controls the card needs before it can preview anything. */
		children,
		ondone
	}: {
		title: string;
		description: string;
		echoLabel: string;
		previewLabel?: string;
		executeLabel: string;
		preview: () => Promise<DryRun | string>;
		execute: (confirm: string) => Promise<string>;
		subject?: unknown;
		ready?: boolean;
		immediate?: boolean;
		prefill?: boolean;
		children?: Snippet;
		ondone?: () => void;
	} = $props();

	let plan = $state.raw<DryRun | null>(null);
	/** The subject the plan on hand actually describes. */
	let planned = $state.raw<string | null>(null);
	let echo = $state('');
	let busy = $state(false);
	let failure = $state<string | null>(null);
	let done = $state<string | null>(null);

	const id = $props.id();
	const fingerprint = $derived(JSON.stringify(subject ?? null));
	/**
	 * The plan, but only while it still describes what is on screen. Editing
	 * the controls above puts the card back to the preview button, because a
	 * count from before the edit is a count for a different question.
	 */
	const showing = $derived(fingerprint === planned ? plan : null);
	/** The button opens only on an exact echo — the server checks it again. */
	const matches = $derived(showing !== null && echo === showing.confirm);
	const rows = $derived(Object.entries(showing?.would_delete ?? {}));
	/**
	 * Whether the answer is "nothing". A column of zeros is technically the
	 * same information and reads as a threat; the sentence reads as the
	 * reassurance it is.
	 */
	const nothing = $derived(rows.every(([, howMany]) => howMany === 0));
	/**
	 * The eval runs that would lose traces (spec 014 #14, spec 035 #6): the
	 * deletion overrides the pin a run puts on them, and the preview is where
	 * the operator sees the hole before it opens.
	 */
	const runs = $derived(showing?.affected_runs ?? []);
	/**
	 * The datasets that would lose items cut from the erased traces, history
	 * and all (spec 044 #9): the one deletion of dataset items the API makes
	 * outside a dataset's own delete, so it is named beside the runs.
	 */
	const datasets = $derived(showing?.affected_datasets ?? []);

	$effect(() => {
		if (immediate && ready && showing === null && !busy && !done && !failure) {
			void run('preview');
		}
	});

	async function run(step: 'preview' | 'execute') {
		busy = true;
		failure = null;
		done = null;
		try {
			if (step === 'preview') {
				// Read before the request, not after: an edit made while it is
				// in flight makes the answer stale the moment it lands.
				const asked = fingerprint;
				const answer = await preview();
				if (typeof answer === 'string') {
					// Nothing to confirm: the server did it, because there was
					// nothing destructive to stop for.
					plan = null;
					done = answer;
					ondone?.();
				} else {
					plan = answer;
					planned = asked;
				}
				echo = prefill && plan ? plan.confirm : '';
			} else {
				done = await execute(echo);
				plan = null;
				planned = null;
				echo = '';
				ondone?.();
			}
		} catch (cause) {
			// The server's own words, verbatim: it knows why it refused, and a
			// paraphrase would be this screen's opinion of why (spec 007 #4).
			failure = cause instanceof ApiError ? cause.message : 'The request failed.';
		} finally {
			busy = false;
		}
	}
</script>

<!-- The card turns red when the server says something would be destroyed, and
     not before: a border that is always alarming stops meaning anything. -->
<div class={['bg-canvas rounded-lg border p-3', showing ? 'border-danger' : 'border-border']}>
	<h3 class="font-medium">{title}</h3>
	<p class="text-muted mt-1 text-sm">{description}</p>

	{#if children}
		<div class="mt-3">{@render children()}</div>
	{/if}

	{#if failure}
		<p role="alert" class="text-danger mt-3 flex items-start gap-2 text-sm">
			<TriangleAlert class="mt-0.5 size-4 shrink-0" />
			{failure}
		</p>
	{/if}

	{#if done}
		<p role="status" class="text-ok mt-3 text-sm">{done}</p>
	{/if}

	{#if showing === null}
		<Button class="mt-3" onclick={() => run('preview')} busy={busy} disabled={!ready}>
			{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
			{previewLabel}
		</Button>
	{:else}
		<div class="border-border bg-surface mt-3 rounded-md border p-3">
			<!-- A user-data erasure's dry run says when one of that user is under
			     way, before the counts it is shrinking (spec 047 #13, #31). -->
			{#if showing.running}
				<p class="text-warn mb-1.5 text-sm" data-testid="erasure-running">
					An erasure of this user is under way — {showing.running.phase ?? showing.running.state},
					<code class="font-mono">{showing.running.id.slice(0, 8)}</code>. The counts below shrink
					as it goes, and confirming follows it rather than starting another.
				</p>
			{/if}
			<p class="text-subtle text-xs font-medium">This would delete</p>
			{#if nothing}
				<p class="text-muted mt-1 text-sm">Nothing — there is no data to remove.</p>
			{:else}
				<dl class="mt-1 flex flex-wrap gap-x-6 gap-y-1">
					{#each rows as [what, howMany] (what)}
						<div class="flex items-baseline gap-1.5">
							<dt class="text-muted text-sm">{what.replaceAll('_', ' ')}</dt>
							<dd class="tabular-nums">{count(howMany)}</dd>
						</div>
					{/each}
				</dl>
			{/if}
			{#if showing.oldest}
				<p class="text-muted mt-1.5 text-sm">
					Reaching back to {timestamp(showing.oldest)}.
				</p>
			{/if}
			{#if runs.length > 0}
				<p class="text-warn mt-1.5 text-sm">
					{runs.length === 1 ? 'An eval run holds some of these' : 'Eval runs hold some of these'}
					and will show them as missing:
					{#each runs as run, index (run.id)}{index > 0 ? ', ' : ' '}<span
							class="whitespace-nowrap">{run.dataset} <code class="font-mono">{run.id.slice(0, 8)}</code
							> ({count(run.traces)})</span
						>{/each}.
				</p>
			{/if}
			{#if datasets.length > 0}
				<p class="text-warn mt-1.5 text-sm">
					{datasets.length === 1 ? 'A dataset loses' : 'Datasets lose'} the items cut from these, every
					version:
					{#each datasets as affected, index (affected.dataset)}{index > 0 ? ', ' : ' '}<span
							class="whitespace-nowrap">{affected.dataset} ({count(affected.items)})</span
						>{/each}.
				</p>
			{/if}
			{#if showing.note}
				<p class="text-muted mt-1.5 text-sm">{showing.note}</p>
			{/if}
		</div>

		<label for="{id}-echo" class="mt-3 mb-1.5 block text-sm font-medium">
			Type the {echoLabel} to confirm: <code class="font-mono">{showing.confirm}</code>
		</label>
		<div class="flex flex-wrap gap-1.5">
			<input
				id="{id}-echo"
				type="text"
				bind:value={echo}
				autocomplete="off"
				spellcheck="false"
				autocapitalize="off"
				aria-describedby="{id}-hint"
				class="border-border bg-canvas placeholder:text-subtle min-w-0 flex-1 rounded-md border
					px-2 py-1 font-mono text-sm"
			/>
			<Button
				variant="primary"
				class="border-danger bg-danger text-on-accent"
				disabled={!matches}
				busy={busy}
				onclick={() => run('execute')}
			>
				{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
				{executeLabel}
			</Button>
			<Button onclick={() => ((plan = null), (planned = null), (echo = ''))}>Cancel</Button>
		</div>
		<p id="{id}-hint" class="text-subtle mt-1 text-xs">
			This cannot be undone from here.
		</p>
	{/if}
</div>
