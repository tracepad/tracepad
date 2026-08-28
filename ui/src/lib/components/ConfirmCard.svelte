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
		children?: Snippet;
		ondone?: () => void;
	} = $props();

	let plan = $state.raw<DryRun | null>(null);
	let echo = $state('');
	let busy = $state(false);
	let failure = $state<string | null>(null);
	let done = $state<string | null>(null);

	const id = $props.id();
	/** The button opens only on an exact echo — the server checks it again. */
	const matches = $derived(plan !== null && echo === plan.confirm);
	const rows = $derived(Object.entries(plan?.would_delete ?? {}));

	async function run(step: 'preview' | 'execute') {
		busy = true;
		failure = null;
		done = null;
		try {
			if (step === 'preview') {
				const answer = await preview();
				if (typeof answer === 'string') {
					// Nothing to confirm: the server did it, because there was
					// nothing destructive to stop for.
					plan = null;
					done = answer;
					ondone?.();
				} else {
					plan = answer;
				}
				echo = '';
			} else {
				done = await execute(echo);
				plan = null;
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
<div class={['bg-canvas rounded-lg border p-3', plan ? 'border-danger' : 'border-border']}>
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

	{#if plan === null}
		<Button class="mt-3" onclick={() => run('preview')} busy={busy}>
			{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
			{previewLabel}
		</Button>
	{:else}
		<div class="border-border bg-surface mt-3 rounded-md border p-3">
			<p class="text-subtle text-xs font-medium">This would delete</p>
			{#if rows.length === 0}
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
			{#if plan.oldest}
				<p class="text-muted mt-1.5 text-sm">
					Reaching back to {timestamp(plan.oldest)}.
				</p>
			{/if}
			{#if plan.note}
				<p class="text-muted mt-1.5 text-sm">{plan.note}</p>
			{/if}
		</div>

		<label for="{id}-echo" class="mt-3 mb-1.5 block text-sm font-medium">
			Type the {echoLabel} to confirm: <code class="font-mono">{plan.confirm}</code>
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
			<Button onclick={() => ((plan = null), (echo = ''))}>Cancel</Button>
		</div>
		<p id="{id}-hint" class="text-subtle mt-1 text-xs">
			This cannot be undone from here.
		</p>
	{/if}
</div>
