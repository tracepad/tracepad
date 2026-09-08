<script lang="ts">
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import Plus from '@lucide/svelte/icons/plus';
	import type { Snippet } from 'svelte';
	import { SvelteSet } from 'svelte/reactivity';
	import { api, type Score, type ScoreConfig } from '$lib/api/client.svelte';
	import { relative, timestampPrecise } from '$lib/format';
	import {
		boundsLabel,
		isCut,
		scoreSource,
		scoreValue,
		type HeaderScore,
		type ScoreTarget
	} from '$lib/scores';
	import Button from '../Button.svelte';
	import ConfirmDialog from '../ConfirmDialog.svelte';
	import ScoreDialog from './ScoreDialog.svelte';

	// The *Scores* block (spec 022 #1–#3): what has been said about the thing
	// on screen, and the one control that says something more. The same
	// component on all three surfaces — the trace header, the observation
	// panel, the session header — because a chip must mean the same thing
	// wherever a reader meets it.
	//
	// It renders what it is handed and re-reads through `onchanged` after a
	// write of its own; the request itself belongs to whoever loaded the
	// target, so a trace costs one score read and not one per panel (#1).

	let {
		scores,
		target,
		configs,
		/** What the button says; an observation names itself (#4). */
		addLabel = 'Score',
		/** Where the heading sits in the outline of the screen around it. */
		level = 2,
		/** What is on screen but not here — the observation scores of a trace. */
		note = null,
		loading = false,
		failure = null,
		/** The target has more scores than one page carries (edge cases). */
		truncated = false,
		/**
		 * What else this surface offers about the thing being read — *Add to
		 * queue* on a trace (spec 024 #13). It rides here rather than in a bar
		 * of its own because this row is already the one control strip a trace
		 * header has, and a second would cost the tree beside it its height.
		 */
		actions,
		onchanged
	}: {
		scores: HeaderScore[];
		target: ScoreTarget;
		configs: ScoreConfig[];
		addLabel?: string;
		level?: 2 | 3;
		note?: string | null;
		loading?: boolean;
		failure?: string | null;
		truncated?: boolean;
		actions?: Snippet;
		onchanged: () => void;
	} = $props();

	const open = new SvelteSet<string>();
	/** Which score the dialog is editing; `undefined` when it is shut. */
	let editing = $state.raw<Score | null | undefined>(undefined);
	let removing = $state.raw<Score | null>(null);

	const config = (name: string) => configs.find((one) => one.name === name);

	async function remove() {
		if (!removing) return;
		await api.deleteScore(removing.id);
		onchanged();
	}
</script>

<section class="border-border shrink-0 border-b px-4 py-2">
	<!-- The heading and the button are outside the scrolling part on purpose:
	     a phone gives every chip a 44-pixel touch target (spec 006 #15), so a
	     trace with eight verdicts would push both off the screen along with
	     the tree. -->
	<div class="flex items-center gap-3">
		<svelte:element
			this={`h${level}`}
			class="text-subtle text-xs font-medium tracking-wide uppercase"
		>
			Scores
		</svelte:element>

		{#if loading && scores.length === 0}
			<span class="text-subtle text-sm">Loading</span>
		{:else if failure}
			<span role="alert" class="text-danger text-sm">{failure}</span>
		{:else if scores.length === 0}
			<span class="text-subtle text-sm">No scores</span>
		{/if}

		{#if note}
			<span class="text-subtle truncate text-sm">{note}</span>
		{/if}
		{#if truncated}
			<span class="text-subtle text-sm">and more</span>
		{/if}

		<div class="ml-auto flex shrink-0 items-center gap-1.5">
			<Button onclick={() => (editing = null)}>
				<Plus class="size-4" />
				{addLabel}
			</Button>
			{@render actions?.()}
		</div>
	</div>

	<!-- Capped above what a typical trace carries, so the cap does not bite on
	     a wide screen and a graded-to-death trace scrolls rather than grows. -->
	<div
		class={[
			'flex max-h-40 flex-wrap items-center gap-x-3 gap-y-1.5 overflow-y-auto',
			scores.length > 0 && 'mt-1.5'
		]}
	>
		{#each scores as { score, unknown } (score.id)}
			{@const shown = open.has(score.id)}
			<div class="border-border bg-surface max-w-full rounded-md border text-sm">
				<button
					type="button"
					aria-expanded={shown}
					onclick={() => (shown ? open.delete(score.id) : open.add(score.id))}
					class="hover:bg-raised pointer-coarse:min-h-11 flex w-full cursor-pointer items-center
						gap-1.5 rounded-md px-2 py-1 text-left transition-colors duration-100"
				>
					{#if shown}
						<ChevronDown class="text-subtle size-3.5 shrink-0" />
					{:else}
						<ChevronRight class="text-subtle size-3.5 shrink-0" />
					{/if}
					<span class="shrink-0 font-medium">{score.name}</span>
					<span
						class="text-accent max-w-60 truncate tabular-nums"
						title={boundsLabel(config(score.name))}
					>
						{scoreValue(score)}
					</span>
					<span class="text-subtle shrink-0 text-xs">{scoreSource(score)}</span>
					<span class="text-subtle shrink-0 text-xs" title={timestampPrecise(score.timestamp)}>
						{relative(score.timestamp)}
					</span>
					{#if unknown}
						<!-- A score naming an observation this trace does not
						     carry: no panel will ever show it, so it is here with
						     what is odd about it said out loud (edge cases). -->
						<span class="text-warn shrink-0 text-xs">unknown observation</span>
					{/if}
				</button>

				{#if shown}
					<div class="border-border space-y-2 border-t px-2 py-2">
						{#if isCut(score)}
							<p class="max-h-40 overflow-auto text-xs break-words whitespace-pre-wrap">
								{scoreValue(score, true)}
							</p>
						{/if}
						{#if score.comment}
							<p class="text-muted text-xs break-words">{score.comment}</p>
						{:else if !isCut(score)}
							<p class="text-subtle text-xs">No comment</p>
						{/if}
						<div class="flex gap-1.5">
							<Button variant="ghost" onclick={() => (editing = score)}>Edit</Button>
							<Button variant="ghost" onclick={() => (removing = score)}>Delete</Button>
						</div>
					</div>
				{/if}
			</div>
		{/each}
	</div>
</section>

<ScoreDialog
	open={editing !== undefined}
	{target}
	{configs}
	score={editing ?? null}
	onclose={() => (editing = undefined)}
	onsaved={onchanged}
/>

<ConfirmDialog
	open={removing !== null}
	title="Delete this score?"
	description={removing
		? `${removing.name} = ${scoreValue(removing)} goes. Posting the same id again would put it back.`
		: ''}
	confirmLabel="Delete the score"
	onconfirm={remove}
	onclose={() => (removing = null)}
/>
