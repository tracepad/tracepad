<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		ApiError,
		api,
		type ScoreConfig,
		type ScoreSeries,
		type ScoreTrends
	} from '$lib/api/client.svelte';
	import {
		axisRange,
		breakdownRows,
		buildScoreSeries,
		figure,
		orderSeries,
		QUALITY_BREAKDOWNS,
		qualitySearch
	} from '$lib/api/quality';
	import { presetRange, readBucket, readRange, type Range } from '$lib/api/range';
	import Button from '$lib/components/Button.svelte';
	import Chart from '$lib/components/Chart.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import RangePicker from '$lib/components/RangePicker.svelte';
	import ScoreBreakdown from '$lib/components/quality/ScoreBreakdown.svelte';
	import { count } from '$lib/format';

	// Quality (spec 025 #9–#12): what the evals and the reviewers have been
	// saying, over the same window the Stats screen answers for.
	//
	// One screen, two views. Without `?name=` it is a card per score name over
	// one request — six names, six sparklines, one glance. With a name it is
	// the Stats screen's own composition for that one score: the trend, the
	// count beside it, and the three breakdowns.
	//
	// Nothing here computes a number the API did not send. The mean of a day is
	// the endpoint's, out of the four numbers a rolled row carries, because a
	// mean of means is a wrong number that looks like a right one.

	/**
	 * When this page was opened, read once. The default window resolves against
	 * it rather than against the clock: a `$derived` reading `new Date()`
	 * produces a different `from` on every URL change, so opening a card would
	 * re-ask with a window a few milliseconds narrower (spec 023's lesson).
	 */
	const openedAt = new Date();
	const range = $derived(readRange(page.url.searchParams));
	/** Thirty days is the window this screen opens on (spec 025 #9). */
	const viewed = $derived(range.from || range.to ? range : presetRange('30d', openedAt));
	const environment = $derived(page.url.searchParams.get('environment')?.trim() ?? '');
	const bucket = $derived(readBucket(page.url.searchParams, viewed, openedAt));
	/** The detail view is the overview with a name in the URL (Decision 11). */
	const name = $derived(page.url.searchParams.get('name')?.trim() ?? '');

	let trends = $state.raw<ScoreTrends | null>(null);
	let breakdowns = $state.raw<(ScoreTrends | null)[]>([]);
	let configs = $state.raw<ScoreConfig[]>([]);
	let loading = $state(true);
	let failure = $state<string | null>(null);
	let generation = $state(0);

	// The configs do not depend on the window, so they are read once: they give
	// the card order, the numeric axis and the description a card wears as its
	// title (Decisions 10, 12).
	$effect(() => {
		const controller = new AbortController();
		api
			.listScoreConfigs(controller.signal)
			.then((list) => (configs = list.configs as ScoreConfig[]))
			.catch(() => {});
		return () => controller.abort();
	});

	/**
	 * The question this screen asks, as a value that compares. Not the objects
	 * behind it: `viewed` is rebuilt on every URL change, and a `$derived`
	 * object is never equal to the last one (spec 023 #9's lesson).
	 */
	const asked = $derived(`${name}|${viewed.from ?? ''}|${viewed.to ?? ''}|${environment}|${bucket}`);

	$effect(() => {
		void asked;
		void generation;
		const controller = new AbortController();
		untrack(() =>
			load(
				{ from: viewed.from, to: viewed.to, environment: environment || undefined },
				name,
				bucket,
				controller.signal
			)
		);
		return () => controller.abort();
	});

	async function load(
		query: { from?: string; to?: string; environment?: string },
		asking: string,
		group: string,
		signal: AbortSignal
	) {
		loading = true;
		failure = null;
		try {
			// The overview is one request; the detail is four — the series and
			// the three groupings (Decision 11).
			const [series, ...rest] = await Promise.all([
				api.getScoreTrends({ ...query, name: asking || undefined, group_by: group }, signal),
				...(asking
					? QUALITY_BREAKDOWNS.map((one) =>
							api.getScoreTrends({ ...query, name: asking, group_by: one.group }, signal)
						)
					: [])
			]);
			if (signal.aborted) return;
			trends = series;
			breakdowns = rest;
		} catch (cause) {
			if (signal.aborted) return;
			trends = null;
			breakdowns = [];
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the score trends.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	const series = $derived(orderSeries((trends?.series ?? []) as ScoreSeries[], configs));
	const configOf = $derived((one: ScoreSeries) => configs.find((config) => config.name === one.name));
	const shapeOf = $derived((one: ScoreSeries) =>
		buildScoreSeries(one, { from: viewed.from, to: viewed.to, bucket, now: openedAt })
	);
	/** The detail view is about one series, and shows every type the name has. */
	const detail = $derived(name ? series : []);
	/** How many scores the detail view is looking at, across its types. */
	const graded = $derived(
		detail.reduce((sum, one) => sum + one.buckets.reduce((n, bucket) => n + bucket.count, 0), 0)
	);

	function navigate(next: Parameters<typeof qualitySearch>[1]) {
		goto(qualitySearch(page.url.searchParams, next), { keepFocus: true, noScroll: true });
	}

	function setRange(picked: Range) {
		navigate({ from: picked.from ?? '', to: picked.to ?? '' });
	}

	/** How a value reads in a legend, by the series' type. */
	function formatter(one: ScoreSeries) {
		if (one.data_type === 'numeric') return (value: number | null | undefined) =>
			typeof value === 'number' ? figure(value) : '—';
		return (value: number | null | undefined) =>
			typeof value === 'number' ? `${Math.round(value * 100)}%` : '—';
	}

	const summaryLabel = (one: ScoreSeries) =>
		one.data_type === 'numeric' ? 'Mean' : one.data_type === 'boolean' ? 'Rate' : 'Categories';
</script>

<svelte:head><title>{name ? `${name} · ` : ''}Quality · Tracepad</title></svelte:head>

<PageHeader title="Quality">
	{#snippet meta()}
		{#if name}
			<a href={qualitySearch(page.url.searchParams, { name: '' })} class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
				<ChevronLeft class="size-3.5" />
				All scores
			</a>
			<span class="truncate font-mono" title={name}>{name}</span>
		{/if}
		{#if loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else if name}
			<span class="tabular-nums whitespace-nowrap">{count(graded)} scores</span>
		{:else}
			<span class="tabular-nums whitespace-nowrap">
				{count(series.length)} score {series.length === 1 ? 'name' : 'names'}
			</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button onclick={() => generation++} busy={loading} title="Read the score trends again">
			<RefreshCw class="size-4" />
			Refresh
		</Button>
	{/snippet}
</PageHeader>

<div class="border-border overflow-x-auto border-b px-4 py-2">
	<div class="flex min-w-0 items-center gap-1.5">
		<RangePicker range={viewed} onchange={setRange} />
		<label class="sr-only" for="quality-environment">Environment</label>
		<input
			id="quality-environment"
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
	{#if series.length === 0 && !loading}
		<!-- A trend of a score nobody has recorded yet is a real answer, and the
		     SDK line is how it starts being recorded (Decision 12). -->
		<div class="mx-auto max-w-lg pt-8">
			<h2 class="font-medium">
				{#if name}
					No score named <code class="font-mono">{name}</code> lands in this window
				{:else}
					No score names a trace in this window
				{/if}
			</h2>
			<p class="text-muted mt-1">
				A trend is drawn from scores attached to a trace — a judge's verdict, an eval's number, a
				reviewer's rating:
			</p>
			<pre
				class="border-border bg-surface mt-2 overflow-x-auto rounded-lg border p-3 font-mono text-xs">tracepad.score(trace_id=…, name="hallucination", value=0.2)</pre>
			<p class="text-muted mt-2">
				Scores that name only a session, and text scores, have nothing to place on a timeline — see
				<a
					href="https://github.com/tracepad/tracepad/blob/main/docs/quality.md"
					class="text-accent hover:underline">the quality guide</a
				>.
			</p>
		</div>
	{:else if name}
		{#each detail as one (one.name + one.data_type)}
			{@const shape = shapeOf(one)}
			<div class="mb-3 grid grid-cols-1 gap-3 lg:grid-cols-3">
				<div class="lg:col-span-2">
					<Chart
						title="{one.name} · {one.data_type}"
						x={shape.x}
						lines={[...shape.primary, ...shape.extremes]}
						format={formatter(one)}
						range={axisRange(configOf(one))}
						summary="{one.name} per {bucket} over {count(shape.total)} scores."
						sync="quality-detail"
						height={220}
					/>
				</div>
				<Chart
					title="Scores"
					x={shape.x}
					lines={[{ label: 'Scores', values: shape.counts, token: 'muted' }]}
					format={(value) => count(value)}
					summary="How many scores of this name fell in each {bucket}."
					sync="quality-detail"
					height={220}
				/>
			</div>
			<div class="grid grid-cols-1 gap-3 lg:grid-cols-3">
				{#each QUALITY_BREAKDOWNS as breakdown, index (breakdown.group)}
					<ScoreBreakdown
						title={breakdown.title}
						label={breakdown.label}
						summaryLabel={summaryLabel(one)}
						note={breakdown.group === 'model'
							? 'Only a score that names an observation has a model.'
							: undefined}
						rows={breakdownRows(
							(breakdowns[index]?.series ?? []).find(
								(candidate) => candidate.name === one.name && candidate.data_type === one.data_type
							) as ScoreSeries | undefined,
							breakdown.group === 'release' ? '(no release)' : '—'
						)}
					/>
				{/each}
			</div>
		{/each}
	{:else}
		<div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
			{#each series as one (one.name + one.data_type)}
				{@const shape = shapeOf(one)}
				<a
					href={qualitySearch(page.url.searchParams, { name: one.name })}
					title={configOf(one)?.description || undefined}
					class="focus-visible:outline-accent block rounded-lg transition-opacity duration-100 hover:opacity-80"
				>
					<Chart
						title="{one.name} · {one.data_type} · {count(shape.total)} scores"
						x={shape.x}
						lines={shape.primary}
						format={formatter(one)}
						range={axisRange(configOf(one))}
						summary="{one.name} per {bucket} over {count(shape.total)} scores. Open for the breakdowns."
						sync="quality-card-{one.name}-{one.data_type}"
						height={120}
					/>
				</a>
			{/each}
		</div>
	{/if}
</div>
