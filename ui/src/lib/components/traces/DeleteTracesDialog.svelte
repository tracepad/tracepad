<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { api, type DryRun } from '$lib/api/client.svelte';
	import type { TraceFilters } from '$lib/api/traces';
	import { count, timestamp } from '$lib/format';
	import Button from '../Button.svelte';
	import ConfirmCard from '../ConfirmCard.svelte';
	import { chips } from '../FilterBar.svelte';

	// *Delete…* on the traces listing (spec 035 #9): the manager's gesture at
	// the surface where the choice is made, beside *Add to queue…*. The dialog
	// names the filters as the bar renders them — its chips — and the count
	// the listing already holds, so a filter that matches ten thousand is seen
	// before anything goes.
	//
	// `to` is pinned to the moment the dialog opened when the filter has none
	// (#2 made visible): what the operator confirms is the set they counted,
	// however much ingest flows in meanwhile. Then rounds (#4): every answer is
	// a complete, consistent act, the line says how far along it is, and *Stop*
	// finishes the round in flight and leaves the rest — a person who sees the
	// number and changes their mind must not need to close the tab.

	let {
		open = false,
		filters,
		/** The listing's own count of what the filters match, as it shows it. */
		matched,
		onclose,
		/** The host's own after: the listing re-reads. */
		ondeleted
	}: {
		open?: boolean;
		filters: TraceFilters;
		matched: string;
		onclose: () => void;
		ondeleted: () => void;
	} = $props();

	/** One round: the server's own default, and its cap. */
	const ROUND = 1000;

	// Read once, when the dialog opens — not derived, because the moment the
	// set was closed at must not move while the dialog is up.
	let pinned = $state.raw<TraceFilters>({});
	$effect(() => {
		if (open) pinned = { ...filters, to: filters.to ?? new Date().toISOString() };
	});
	const pinnedHere = $derived(filters.to === undefined);
	const named = $derived(chips(filters));

	/** How far the rounds have come, while they run and after they stop. */
	let progress = $state<{ deleted: number; of: number } | null>(null);
	let stopping = $state(false);
	let running = $state(false);

	async function ask(confirm?: string): Promise<DryRun | string> {
		if (confirm === undefined) {
			progress = null;
			const answer = await api.deleteTraces(pinned);
			if (!answer.dry_run) return 'Nothing was deleted.';
			return answer;
		}
		return rounds(confirm);
	}

	/**
	 * The client is the one that loops (#4): the interface's thirty-second
	 * clock would cut off a request that ran for minutes, and then the operator
	 * sees an error while the store is in fact fine.
	 */
	async function rounds(confirm: string): Promise<string> {
		running = true;
		stopping = false;
		let deleted = 0;
		try {
			for (;;) {
				const answer = await api.deleteTraces(pinned, confirm, ROUND);
				if (answer.dry_run) return 'Nothing was deleted.';
				deleted += answer.deleted.traces ?? 0;
				progress = { deleted, of: progress?.of ?? deleted };
				if (!answer.more) return `Deleted ${count(deleted)} traces.`;
				if (stopping) {
					return `Stopped after ${count(deleted)} traces; the rest are still there.`;
				}
			}
		} finally {
			running = false;
		}
	}

	/** The dry run's exact count is the denominator the progress line shows. */
	function planned(plan: DryRun | string) {
		if (typeof plan !== 'string' && plan.matched !== undefined) {
			progress = { deleted: 0, of: plan.matched };
		}
		return plan;
	}
</script>

<Dialog.Root {open} onOpenChange={(next) => !next && !running && onclose()}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="fixed top-1/2 left-1/2 z-50 max-h-[90dvh] w-[min(32rem,calc(100vw-1.5rem))]
				-translate-x-1/2 -translate-y-1/2 overflow-y-auto"
			interactOutsideBehavior={running ? 'ignore' : 'close'}
			escapeKeydownBehavior={running ? 'ignore' : 'close'}
		>
			<Dialog.Title class="sr-only">Delete the traces these filters match</Dialog.Title>
			<Dialog.Description class="sr-only">
				Names the filters, shows what deleting their matches would remove and asks for the
				project's name.
			</Dialog.Description>
			<ConfirmCard
				title="Delete the traces these filters match"
				description="Every matching trace with its observations, scores, payloads and queue items,
					newest first, in rounds. Raw OTLP bodies are not touched."
				echoLabel="project name"
				previewLabel="Show what would go"
				executeLabel="Delete these traces"
				subject={pinned}
				immediate
				preview={() => ask().then(planned)}
				execute={(confirm) => ask(confirm) as Promise<string>}
				ondone={ondeleted}
			>
				<!-- The filters as the bar shows them (#12), and the moment the
				     set is closed at — said in so many words when this dialog
				     chose it, because an empty filter is *every trace before now*
				     and the wording has to make that plain. -->
				<p class="text-sm">
					<span class="text-muted">{matched} on the listing match</span>
					{#if named.length > 0}
						<span class="mt-1 flex flex-wrap gap-1">
							{#each named as label (label.name)}
								<span
									title={label.title}
									class="border-border bg-surface text-muted rounded-md border px-2 py-0.5 text-sm
										whitespace-nowrap"
								>
									{label.text}
								</span>
							{/each}
						</span>
					{:else if !filters.q && !filters.from}
						<span class="text-muted"> — <strong class="text-fg">no filter at all</strong></span>
					{/if}
				</p>
				<p class="text-muted mt-1.5 text-sm">
					{#if filters.q}Searching for <code class="font-mono">{filters.q}</code>;{/if}
					{#if filters.from}started since {timestamp(filters.from)},{/if}
					{#if pinnedHere}
						traces that started before <strong class="text-fg">{timestamp(pinned.to)}</strong>,
						the moment this dialog opened — what comes in afterwards is not part of it.
					{:else}
						traces that started before <strong class="text-fg">{timestamp(pinned.to)}</strong>.
					{/if}
				</p>
			</ConfirmCard>

			{#if progress && (running || progress.deleted > 0)}
				<div
					class="border-border bg-canvas mt-2 flex flex-wrap items-center justify-between gap-2
						rounded-lg border px-3 py-2 text-sm"
				>
					<p role="status" aria-label="Progress" class="tabular-nums">
						{count(progress.deleted)} of {count(progress.of)} deleted{running ? '…' : ''}
					</p>
					{#if running}
						<Button onclick={() => (stopping = true)} disabled={stopping}>
							{stopping ? 'Stopping after this round' : 'Stop'}
						</Button>
					{/if}
				</div>
			{/if}
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
