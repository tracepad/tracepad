<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { ApiError, api, type RunAttempt, type RunItem } from '$lib/api/client.svelte';
	import { scoreText, short } from '$lib/evals';
	import { ABSENT, cost, duration, timestamp } from '$lib/format';
	import Payload, { isTruncated } from '../Payload.svelte';

	// One case of a run: what was expected, and every attempt the run made at
	// it — what it cost, how it went, what it answered, how it was scored
	// (spec 014 API contract → Runs). An attempt's trace is one click deeper,
	// in the panel (spec 016 #12); this component only says which.
	//
	// It renders the listing's row rather than reading the run's item view
	// again: the view has no single-item address, and the row already holds
	// everything but the payloads the budget cut — which are loaded from where
	// each of them lives whole.

	let {
		item,
		dataset,
		version,
		ondrill
	}: {
		/** The row on the page, or null when the panel names a row the page does not hold. */
		item: RunItem | null;
		dataset: string;
		version: number;
		ondrill: (traceID: string) => void;
	} = $props();

	// The expected output, whole, when the budget cut it: the dataset item is
	// budget-exempt (spec 014 #19), so the whole of it is one read away.
	let expected = $state.raw<{ id: string; value: unknown } | null>(null);
	let expectedLoading = $state(false);
	// Each attempt's output, whole, by trace id — from the one budget-exempt
	// observation endpoint the marker names.
	let outputs = $state.raw<Record<string, unknown>>({});
	let loadingOutput = $state<string | null>(null);
	let failure = $state<string | null>(null);

	async function loadExpected() {
		if (!item?.id || expectedLoading) return;
		expectedLoading = true;
		failure = null;
		try {
			const whole = await api.getItem(dataset, item.id, version);
			expected = { id: item.id, value: whole.expected_output };
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the item.';
		} finally {
			expectedLoading = false;
		}
	}

	async function loadOutput(attempt: RunAttempt) {
		const marker = isTruncated(attempt.output) ? attempt.output : null;
		if (!marker || loadingOutput) return;
		loadingOutput = attempt.trace_id;
		failure = null;
		try {
			const whole = await api.getObservationIO(marker.observation_id, marker.trace_id);
			outputs = { ...outputs, [attempt.trace_id]: whole.output };
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the payload.';
		} finally {
			loadingOutput = null;
		}
	}

	const expectedValue = $derived(
		expected?.id === item?.id ? expected?.value : item?.expected_output
	);
</script>

{#if !item}
	<div class="flex flex-1 items-start justify-center p-8">
		<p class="text-subtle max-w-md text-center">
			This item is not on the page any more. Turn back to it, or open the case in its dataset.
		</p>
	</div>
{:else}
	<div class="min-h-0 flex-1 overflow-auto">
		{#if failure}
			<p role="alert" class="text-danger bg-danger-soft flex items-center gap-2 px-4 py-2">
				<TriangleAlert class="size-4 shrink-0" />
				{failure}
			</p>
		{/if}

		{#if item.unknown}
			<p class="text-warn border-border border-b px-4 py-2 text-sm">
				{#if item.id}
					The run's traces named item {short(item.id)}, which the dataset does not have at version
					{version}.
				{:else}
					These traces named the run and no item.
				{/if}
			</p>
		{:else}
			<Payload
				label="Expected output"
				value={expectedValue}
				loading={expectedLoading}
				onload={loadExpected}
			/>
		{/if}

		<section class="border-border border-t px-4 py-3">
			<h3 class="text-muted mb-2 text-xs font-medium tracking-wide uppercase">
				{item.attempts.length === 1 ? '1 attempt' : `${item.attempts.length} attempts`}
			</h3>
			{#if item.attempts.length === 0}
				<p class="text-subtle">{ABSENT} No trace of the run named this case.</p>
			{/if}
			{#each item.attempts as attempt (attempt.trace_id)}
				<article class="border-border mb-3 rounded-md border">
					<header class="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm">
						<!-- The trace, one level down in this same panel (spec 016 #12). -->
						<button
							type="button"
							onclick={() => ondrill(attempt.trace_id)}
							class="text-accent cursor-pointer font-mono text-xs underline underline-offset-2"
							title="Open the trace in this panel"
						>
							{attempt.trace_id}
						</button>
						<span class="text-subtle font-mono text-xs tabular-nums">{timestamp(attempt.timestamp)}</span>
						<span class="text-muted tabular-nums">{duration(attempt.latency_ms)}</span>
						<span class="text-muted tabular-nums">{cost(attempt.total_cost)}</span>
						{#if attempt.error_count > 0}
							<span class="text-danger bg-danger-soft rounded px-1.5 py-0.5 text-xs font-medium">
								{attempt.error_count}
								{attempt.error_count === 1 ? 'error' : 'errors'}
							</span>
						{/if}
					</header>
					{#if attempt.scores.length > 0}
						<ul class="flex flex-wrap gap-1.5 px-3 pb-2">
							{#each attempt.scores as score (score.id)}
								<li
									class="border-border bg-surface rounded border px-1.5 py-0.5 text-xs"
									title={score.comment ?? undefined}
								>
									<span class="text-muted">{score.name}</span>
									<span class="font-medium tabular-nums">
										{scoreText(score.value ?? score.string_value)}
									</span>
								</li>
							{/each}
						</ul>
					{/if}
					<Payload
						label="Output"
						value={attempt.trace_id in outputs ? outputs[attempt.trace_id] : attempt.output}
						loading={loadingOutput === attempt.trace_id}
						onload={() => loadOutput(attempt)}
					/>
				</article>
			{/each}
		</section>
	</div>
{/if}
