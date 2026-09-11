<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api, type Stats } from '$lib/api/client.svelte';
	import { DEFAULT_PRESET, presetRange, readBucket, readRange, type Range } from '$lib/api/range';
	import { breakdown, buildSeries, type StatsBucket } from '$lib/api/stats';
	import BreakdownTable from '$lib/components/BreakdownTable.svelte';
	import Button from '$lib/components/Button.svelte';
	import Chart from '$lib/components/Chart.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import RangePicker from '$lib/components/RangePicker.svelte';
	import { cost, count, duration } from '$lib/format';
	import { href } from '$lib/project.svelte';

	// Stats over `GET /api/v1/stats`: four time series and two categorical
	// breakdowns, all of them the endpoint's own numbers. Nothing is computed
	// here that the API did not send, except the x axis — time passes whether
	// or not anything was traced, and a bucket with no row stays a gap.
	//
	// Three requests, because `group_by` is one dimension at a time: the
	// timeline, then the model and environment breakdowns.

	const range = $derived(readRange(page.url.searchParams));
	const environment = $derived(page.url.searchParams.get('environment')?.trim() ?? '');
	const bucket = $derived(readBucket(page.url.searchParams, range, new Date()));
	/** Whether the bucket in the URL was chosen rather than derived. */
	const chosen = $derived(page.url.searchParams.get('group_by'));

	let timeline = $state.raw<StatsBucket[]>([]);
	let models = $state.raw<Stats | null>(null);
	let environments = $state.raw<Stats | null>(null);
	// "Did cost or latency move with the release" is a chart, not a list
	// (spec 012 #4), and it is the same shape as the two breakdowns beside it.
	let releases = $state.raw<Stats | null>(null);
	let loading = $state(true);
	let failure = $state<string | null>(null);
	let generation = $state(0);

	$effect(() => {
		// The window is part of the link, so a screen opened without one is
		// given the default before it asks for anything (spec 007 #7).
		if (!range.from && !range.to) {
			goto(search({ ...presetRange(DEFAULT_PRESET, new Date()) }), { replaceState: true });
			return;
		}
		// Read here rather than inside `load`: everything the question is made
		// of has to be a dependency of this effect, and a read after the first
		// `await` would not be one.
		const query = { ...range, environment: environment || undefined };
		const group = bucket;
		generation;
		const controller = new AbortController();
		load(query, group, controller.signal);
		return () => controller.abort();
	});

	async function load(
		query: { from?: string; to?: string; environment?: string },
		group: string,
		signal: AbortSignal
	) {
		loading = true;
		failure = null;
		try {
			const [series, byModel, byEnvironment, byRelease] = await Promise.all([
				api.getStats({ ...query, group_by: group }, signal),
				api.getStats({ ...query, group_by: 'model' }, signal),
				api.getStats({ ...query, group_by: 'environment' }, signal),
				api.getStats({ ...query, group_by: 'release' }, signal)
			]);
			if (signal.aborted) return;
			timeline = series.buckets as StatsBucket[];
			models = byModel;
			environments = byEnvironment;
			releases = byRelease;
		} catch (cause) {
			if (signal.aborted) return;
			timeline = [];
			models = null;
			environments = null;
			releases = null;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the statistics.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	const series = $derived(
		buildSeries(timeline, { from: range.from, to: range.to, bucket, now: new Date() })
	);
	const totals = $derived({
		traces: sum(series.count),
		errors: sum(series.errors),
		cost: series.cost.some((value) => value !== null) ? sum(series.cost) : null
	});

	function sum(values: (number | null)[]): number {
		return values.reduce((carry: number, value) => carry + (value ?? 0), 0);
	}

	/** Everything this screen keeps in the URL, in one place. */
	function search(next: Partial<{ from: string; to: string; environment: string; group_by: string }>): string {
		const params = new URLSearchParams();
		const state = {
			from: range.from,
			to: range.to,
			environment,
			group_by: chosen ?? undefined,
			...next
		};
		for (const [name, value] of Object.entries(state)) {
			if (value) params.set(name, value);
		}
		return href('/stats', params);
	}

	function navigate(next: Parameters<typeof search>[0]) {
		goto(search(next), { keepFocus: true });
	}

	// A window change leaves an explicitly chosen bucket alone: somebody who
	// asked for hours meant it, and silently switching them to days on the next
	// preset would be the screen overruling them. Only an unchosen bucket
	// follows the window (spec 007 #6).
	function setRange(picked: Range) {
		navigate({ from: picked.from ?? '', to: picked.to ?? '' });
	}
</script>

<svelte:head><title>Stats · Tracepad</title></svelte:head>

<PageHeader title="Stats">
	{#snippet meta()}
		{#if loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else}
			<span class="tabular-nums">
				{count(totals.traces)} traces · {count(totals.errors)} with errors · {cost(totals.cost)}
			</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button onclick={() => generation++} busy={loading} title="Read the statistics again">
			<RefreshCw class="size-4" />
			Refresh
		</Button>
	{/snippet}
</PageHeader>

<div class="border-border overflow-x-auto border-b px-4 py-2">
	<div class="flex min-w-0 items-center gap-1.5">
		<RangePicker {range} onchange={setRange} />
		<label class="sr-only" for="stats-environment">Environment</label>
		<input
			id="stats-environment"
			type="text"
			value={environment}
			onchange={(event) => navigate({ environment: event.currentTarget.value.trim() })}
			placeholder="Environment"
			autocomplete="off"
			spellcheck="false"
			class="border-border bg-canvas placeholder:text-subtle w-40 rounded-md border px-2 py-1 text-sm"
		/>
		<div class="flex items-center gap-1" role="group" aria-label="Bucket size">
			{#each ['hour', 'day'] as const as size (size)}
				<Button
					variant={bucket === size ? 'primary' : 'default'}
					aria-pressed={bucket === size}
					onclick={() => navigate({ group_by: size })}
				>
					{size === 'hour' ? 'Hourly' : 'Daily'}
				</Button>
			{/each}
		</div>
	</div>
</div>

{#if failure}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{failure}
	</p>
{/if}

<div class="min-h-0 flex-1 overflow-auto p-4">
	<div class="grid grid-cols-1 gap-3 lg:grid-cols-2">
		<Chart
			title="Traces"
			x={series.x}
			lines={[{ label: 'Traces', values: series.count, token: 'accent' }]}
			format={(value) => count(value)}
			summary="{count(totals.traces)} traces across {series.x.length} {bucket} buckets."
		/>
		<Chart
			title="Cost"
			x={series.x}
			lines={[{ label: 'Cost', values: series.cost, token: 'ok' }]}
			format={(value) => cost(value)}
			summary="{cost(totals.cost)} in total; a bucket where nothing reported a cost is a gap, not a zero."
		/>
		<Chart
			title="Latency"
			x={series.x}
			lines={[
				{ label: 'p50', values: series.p50, token: 'accent' },
				{ label: 'p95', values: series.p95, token: 'warn' }
			]}
			format={(value) => duration(value)}
			summary="Median and 95th percentile latency per {bucket}."
		/>
		<Chart
			title="Errors"
			x={series.x}
			lines={[{ label: 'Errors', values: series.errors, token: 'danger' }]}
			format={(value) => count(value)}
			summary="{count(totals.errors)} traces with a failed observation."
		/>
	</div>

	<div class="mt-3 grid grid-cols-1 gap-3 lg:grid-cols-2">
		<BreakdownTable
			title="By model"
			label="Model"
			unit={models?.unit ?? 'observation'}
			rows={breakdown((models?.buckets ?? []) as StatsBucket[])}
		/>
		<BreakdownTable
			title="By environment"
			label="Environment"
			unit={environments?.unit ?? 'trace'}
			rows={breakdown((environments?.buckets ?? []) as StatsBucket[])}
		/>
		<BreakdownTable
			title="By release"
			label="Release"
			unit={releases?.unit ?? 'trace'}
			rows={breakdown((releases?.buckets ?? []) as StatsBucket[], '(no release)')}
		/>
	</div>
</div>
