<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import {
		ApiError,
		api,
		type ComparedItem,
		type RunComparison,
		type Trace
	} from '$lib/api/client.svelte';
	import { deltaText, scoreText } from '$lib/evals';
	import { ABSENT } from '$lib/format';
	import Payload, { isTruncated } from '../Payload.svelte';
	import VerdictChip from './VerdictChip.svelte';

	// One case as two runs answered it (spec 016 #10): the expected output,
	// output A, output B, and the per-name pair with its verdict. The pair and
	// the verdict are the comparison's own row; the three payloads are read
	// from where each lives (spec 016 #17) — the item from its dataset at the
	// version the run pinned, whole; each side's output from the newest trace
	// the run made at this case, through the trace read the panel already
	// uses, so a cut payload arrives with the marker the banner loads.

	let {
		item,
		compared
	}: {
		/** The row on the page, or null when the panel names a row the page does not hold. */
		item: ComparedItem | null;
		compared: RunComparison;
	} = $props();

	type Side = { trace: Trace | null; attempts: number; output: unknown; loading: boolean };
	const fresh = (): Side => ({ trace: null, attempts: 0, output: undefined, loading: false });

	let expected = $state.raw<unknown>(undefined);
	let sides = $state.raw<{ a: Side; b: Side }>({ a: fresh(), b: fresh() });
	let loading = $state(true);
	let failure = $state<string | null>(null);

	$effect(() => {
		const wanted = item?.id ?? null;
		const controller = new AbortController();
		if (wanted) void load(wanted, controller.signal);
		return () => controller.abort();
	});

	async function load(id: string, signal: AbortSignal) {
		loading = true;
		failure = null;
		expected = undefined;
		sides = { a: fresh(), b: fresh() };
		// The item as the run that had it saw it: a's version unless only b's
		// version holds the case.
		const version =
			item?.in === 'only_in_version_b'
				? compared.b.dataset_version
				: compared.a.dataset_version;
		try {
			const [row, a, b] = await Promise.all([
				api.getItem(compared.dataset, id, version, signal),
				side(compared.a.id, id, signal),
				side(compared.b.id, id, signal)
			]);
			if (signal.aborted) return;
			expected = row.expected_output;
			sides = { a, b };
		} catch (cause) {
			if (signal.aborted) return;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the case.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	/** One run's newest attempt at the case, with its root observation's output. */
	async function side(run: string, id: string, signal: AbortSignal): Promise<Side> {
		const page = await api.listTraces({ run_id: run, item_id: id }, { limit: 1, count: true }, signal);
		const newest = page.traces[0];
		if (!newest) return fresh();
		const trace = await api.getTrace(newest.id, signal);
		return {
			trace,
			attempts: page.total ?? page.traces.length,
			output: trace.observations?.[0]?.output,
			loading: false
		};
	}

	async function loadOutput(which: 'a' | 'b') {
		const current = sides[which];
		const marker = isTruncated(current.output) ? current.output : null;
		if (!marker || current.loading) return;
		sides = { ...sides, [which]: { ...current, loading: true } };
		try {
			const whole = await api.getObservationIO(marker.observation_id, marker.trace_id);
			sides = { ...sides, [which]: { ...current, output: whole.output, loading: false } };
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the payload.';
			sides = { ...sides, [which]: { ...current, loading: false } };
		}
	}

	const names = $derived(item ? Object.keys(item.scores).sort() : []);
</script>

{#if !item}
	<div class="flex flex-1 items-start justify-center p-8">
		<p class="text-subtle max-w-md text-center">
			This case is not on the page any more. Turn back to it to read the comparison.
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

		<section class="border-border border-b px-4 py-3">
			<h3 class="text-muted mb-2 text-xs font-medium tracking-wide uppercase">Scores</h3>
			{#if names.length === 0}
				<p class="text-subtle">{ABSENT} No score name both runs gave this case.</p>
			{:else}
				<table class="w-full border-collapse text-left text-sm">
					<thead class="text-subtle text-xs">
						<tr>
							<th scope="col" class="py-1 pr-3 font-medium">Name</th>
							<th scope="col" class="py-1 pr-3 text-right font-medium">A</th>
							<th scope="col" class="py-1 pr-3 text-right font-medium">B</th>
							<th scope="col" class="py-1 pr-3 text-right font-medium">Delta</th>
							<th scope="col" class="py-1 font-medium">Verdict</th>
						</tr>
					</thead>
					<tbody>
						{#each names as name (name)}
							{@const score = item.scores[name]}
							<tr class="border-border border-t">
								<td class="py-1 pr-3">{name}</td>
								<td class="py-1 pr-3 text-right tabular-nums">{scoreText(score.a)}</td>
								<td class="py-1 pr-3 text-right tabular-nums">{scoreText(score.b)}</td>
								<td class="py-1 pr-3 text-right tabular-nums">{deltaText(score.delta)}</td>
								<td class="py-1"><VerdictChip verdict={score.verdict} /></td>
							</tr>
						{/each}
					</tbody>
				</table>
			{/if}
		</section>

		{#if loading}
			<div class="text-subtle flex items-center justify-center gap-2 p-8">
				<LoaderCircle class="size-4 animate-spin" />
				Loading the case
			</div>
		{:else}
			<Payload label="Expected output" value={expected} loading={false} onload={() => {}} />
			{#each [['a', 'Output A', compared.a], ['b', 'Output B', compared.b]] as const as [which, label, run] (which)}
				{@const current = sides[which]}
				<div class="border-border border-t">
					<p class="text-subtle flex flex-wrap items-center gap-x-2 px-4 pt-3 text-xs">
						<span class="font-medium">{run.name ?? run.id.slice(0, 8)}</span>
						{#if current.trace}
							<a
								class="text-accent font-mono underline underline-offset-2"
								href="/traces/{current.trace.id}"
							>
								{current.trace.id}
							</a>
							{#if current.attempts > 1}
								<a
									class="underline underline-offset-2"
									href="/traces?run_id={run.id}&item_id={item.id}"
								>
									newest of {current.attempts} attempts
								</a>
							{/if}
						{:else}
							<span>no attempt</span>
						{/if}
					</p>
					<Payload
						{label}
						value={current.output}
						loading={current.loading}
						onload={() => loadOutput(which)}
					/>
				</div>
			{/each}
		{/if}
	</div>
{/if}
