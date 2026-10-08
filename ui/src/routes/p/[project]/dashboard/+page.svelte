<script lang="ts">
	import Eye from '@lucide/svelte/icons/eye';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
	import SlidersHorizontal from '@lucide/svelte/icons/sliders-horizontal';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { MediaQuery } from 'svelte/reactivity';
	import type { Action, ActionReturn } from 'svelte/action';
	import { flip } from 'svelte/animate';
	import {
		dragHandleZone,
		type DndEvent,
		type DndZoneAttributes,
		type Options
	} from 'svelte-dnd-action';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		ApiError,
		api,
		type ScoreConfig,
		type ScoreSeries,
		type Stats,
		type TraceRow
	} from '$lib/api/client.svelte';
	import { busiest } from '$lib/api/quality';
	import {
		DEFAULT_PRESET,
		TIMELINES,
		minutesFit,
		presetRange,
		previousRange,
		readRange,
		readTimeline,
		type Range,
		type Timeline
	} from '$lib/api/range';
	import { breakdown, buildSeries, summarize, type StatsBucket } from '$lib/api/stats';
	import BreakdownTable from '$lib/components/BreakdownTable.svelte';
	import Button from '$lib/components/Button.svelte';
	import Chart from '$lib/components/Chart.svelte';
	import DashboardBlock from '$lib/components/dashboard/DashboardBlock.svelte';
	import OnboardingCard from '$lib/components/dashboard/OnboardingCard.svelte';
	import SummaryTile from '$lib/components/dashboard/SummaryTile.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import QualityCards from '$lib/components/quality/QualityCards.svelte';
	import RangePicker from '$lib/components/RangePicker.svelte';
	import {
		BLOCKS,
		CHARTS,
		DEFAULT_ARRANGEMENT,
		label,
		reordered,
		type Arrangement,
		type BlockId
	} from '$lib/dashboard';
	import { axisDuration, cost, count, relative, timestamp } from '$lib/format';
	import { dashboard, setDashboard } from '$lib/preferences.svelte';
	import { STILL } from '$lib/phone';
	import { href, project } from '$lib/project.svelte';
	import { rememberedRange, rememberRange } from '$lib/range.svelte';

	// The project's front page (spec 034): the Stats screen with a summary
	// row above its charts, the quality cards below them, the moment the last
	// trace arrived, and a fresh project's first instructions. Every number
	// is the endpoint's own; the one computation here is the change between
	// two windows, which is a subtraction the API deliberately does not do.
	//
	// The blocks are one list in the account's order, and a hidden block
	// makes no request of its own: the timeline serves the five charts and is
	// read while any of them shows.

	/** How many quality cards the block holds (Decision 5). */
	const QUALITY_CARDS = 6;

	const range = $derived(readRange(page.url.searchParams));
	const environment = $derived(page.url.searchParams.get('environment')?.trim() ?? '');
	/** Whether the bucket in the URL was chosen rather than derived. */
	const chosen = $derived(page.url.searchParams.get('group_by'));

	let arrangement = $state<Arrangement>(dashboard(project.id ?? ''));
	let customizing = $state(false);
	/** What the server said when it refused an arrangement, shown in the strip. */
	let saveFailure = $state<string | null>(null);

	const visible = $derived(arrangement.order.filter((id) => !arrangement.hidden.includes(id)));
	/**
	 * Which blocks show, as a value that compares: a drop reorders `visible`
	 * without changing what is asked for, and a fresh array would re-ask.
	 */
	const showing = $derived([...visible].sort().join(','));
	const shows = (id: BlockId) => visible.includes(id);

	let summary = $state.raw<{ now: StatsBucket | null; before: StatsBucket | null } | null>(null);
	let timeline = $state.raw<StatsBucket[]>([]);
	let models = $state.raw<Stats | null>(null);
	let environments = $state.raw<Stats | null>(null);
	let releases = $state.raw<Stats | null>(null);
	let scores = $state.raw<ScoreSeries[]>([]);
	let scoresOmitted = $state(0);
	let configs = $state.raw<ScoreConfig[]>([]);
	/** The newest trace: undefined until read, null when the listing is empty. */
	let lastTrace = $state.raw<TraceRow | null | undefined>(undefined);
	let loading = $state(true);
	let failure = $state<string | null>(null);
	let generation = $state(0);

	/**
	 * The instant the screen last asked. An open window grows while the page
	 * stays open, and a day read by the minute stops being one: the size, the
	 * *Minutely* button and the request are all read against this one clock,
	 * set each time the statistics are asked for, so none of them can go on
	 * believing a window still fits that the server would refuse.
	 */
	let clock = $state(new Date());
	const bucket = $derived(readTimeline(page.url.searchParams, range, clock));
	/**
	 * The quality cards' size. The score rollup has no minutes, so a minute
	 * dashboard draws its cards by the hour (spec 034 #15).
	 */
	const scoreBucket = $derived(bucket === 'minute' ? 'hour' : bucket);
	const SIZE_LABELS: Record<Timeline, string> = { minute: 'Minutely', hour: 'Hourly', day: 'Daily' };

	$effect(() => {
		// The window is part of the link, so a screen opened without one is
		// given one before it asks for anything (spec 007 #7): the window this
		// browser remembers (spec 034 #7), else the default preset.
		if (!range.from && !range.to) {
			const now = new Date();
			goto(search({ ...(rememberedRange(now) ?? presetRange(DEFAULT_PRESET, now)) }), {
				replaceState: true
			});
			return;
		}
		// Read here rather than inside `load`: everything the question is made
		// of has to be a dependency of this effect, and a read after the first
		// `await` would not be one.
		const query = { ...range, environment: environment || undefined };
		const now = new Date();
		clock = now;
		const group = readTimeline(page.url.searchParams, range, now);
		const scoreGroup = group === 'minute' ? 'hour' : group;
		const wanted = showing.split(',') as BlockId[];
		generation;
		const controller = new AbortController();
		load(query, group, scoreGroup, wanted, controller.signal);
		return () => controller.abort();
	});

	async function load(
		query: { from?: string; to?: string; environment?: string },
		group: string,
		scoreGroup: string,
		wanted: BlockId[],
		signal: AbortSignal
	) {
		loading = true;
		failure = null;
		const wants = (id: BlockId) => wanted.includes(id);
		const skip = Promise.resolve(null);
		const totals = (window: Range | null) =>
			window
				? api
						.getStats({ ...query, ...window, group_by: 'total' }, signal)
						.then((stats) => (stats.buckets[0] as StatsBucket | undefined) ?? null)
				: skip;
		try {
			const [newest, now, before, series, byModel, byEnvironment, byRelease, trends] =
				await Promise.all([
					// No window (Decision 4): whether anything is arriving at all
					// is a question about the project, not about the range.
					api.listTraces({ environment: query.environment }, { limit: 1 }, signal),
					wants('summary') ? totals(query) : skip,
					wants('summary') ? totals(previousRange(query, new Date())) : skip,
					CHARTS.some(wants) ? api.getStats({ ...query, group_by: group }, signal) : skip,
					wants('models') ? api.getStats({ ...query, group_by: 'model' }, signal) : skip,
					wants('environments') ? api.getStats({ ...query, group_by: 'environment' }, signal) : skip,
					wants('releases') ? api.getStats({ ...query, group_by: 'release' }, signal) : skip,
					wants('quality') ? api.getScoreTrends({ ...query, group_by: scoreGroup }, signal) : skip
				]);
			if (signal.aborted) return;
			lastTrace = newest.traces[0] ?? null;
			summary = wants('summary') ? { now, before } : null;
			timeline = (series?.buckets ?? []) as StatsBucket[];
			models = byModel;
			environments = byEnvironment;
			releases = byRelease;
			scores = (trends?.series ?? []) as ScoreSeries[];
			scoresOmitted = trends?.omitted ?? 0;
		} catch (cause) {
			if (signal.aborted) return;
			lastTrace = undefined;
			summary = null;
			timeline = [];
			models = null;
			environments = null;
			releases = null;
			scores = [];
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the statistics.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	// The score configs give a card its axis and its description, and do not
	// depend on the window, so they are read once and only when the block shows.
	$effect(() => {
		if (!shows('quality')) return;
		const controller = new AbortController();
		api
			.listScoreConfigs(controller.signal)
			.then((list) => (configs = list.configs as ScoreConfig[]))
			.catch(() => {});
		return () => controller.abort();
	});

	const series = $derived(
		buildSeries(timeline, { from: range.from, to: range.to, bucket, now: new Date() })
	);
	const figures = $derived(summarize(summary?.now ?? null, summary?.before ?? null));
	const cards = $derived(busiest(scores, QUALITY_CARDS));
	/**
	 * A fresh project: the listing is empty and nothing failed (Decision 6).
	 * The listing carries the environment filter (Decision 4), so an empty
	 * answer under one says "none in this environment", not "none at all" —
	 * that is the header's *No traces yet* and empty charts, never the
	 * instructions.
	 */
	const fresh = $derived(lastTrace === null && !failure && !environment);

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
		return href('/dashboard', params);
	}

	function navigate(next: Parameters<typeof search>[0]) {
		goto(search(next), { keepFocus: true });
	}

	// A window change leaves an explicitly chosen bucket alone: somebody who
	// asked for hours meant it, and silently switching them to days on the next
	// preset would be the screen overruling them. Only an unchosen bucket
	// follows the window (spec 007 #6).
	function setRange(picked: Range) {
		rememberRange(picked, new Date());
		navigate({ from: picked.from ?? '', to: picked.to ?? '' });
	}

	// --- Customize (Decision 8, Decision 9) ---------------------------------

	/** No flip while dragging for a person who asked for less motion. */
	const still = new MediaQuery(STILL);
	const flipDurationMs = $derived(still.current ? 0 : 150);
	/**
	 * The blocks on screen as the drag zone holds them; a drag in progress
	 * reorders this copy. The quality block is not drawn while the window
	 * holds no score (Decision 5) — except in Customize, where it has to be
	 * there to be moved or hidden.
	 */
	let items = $state<Block[]>([]);
	$effect(() => {
		items = visible
			.filter((id) => id !== 'quality' || cards.length > 0 || customizing)
			.map((id) => ({ id }));
	});

	async function keep(next: Arrangement | null) {
		arrangement = next ?? DEFAULT_ARRANGEMENT;
		saveFailure = await setDashboard(project.id ?? '', next);
	}

	const hide = (id: BlockId) =>
		keep({ ...arrangement, hidden: [...arrangement.hidden, id] });
	const show = (id: BlockId) =>
		keep({ ...arrangement, hidden: arrangement.hidden.filter((one) => one !== id) });

	function dropped(event: CustomEvent<DndEvent<Block>>) {
		items = event.detail.items;
		keep(reordered(arrangement, items.map((item) => item.id)));
	}

	/**
	 * The drag zone, mounted only in Customize mode: outside it the grid is a
	 * grid, with no list role, no instructions and no handles. The action is
	 * given the list itself, so every change of it — a drag in progress, a
	 * drop, a block hidden — reaches the zone through the action's update.
	 */
	type Block = { id: BlockId };
	const zone: Action<HTMLElement, { active: boolean; items: Block[] }, DndZoneAttributes<Block>> = (
		node,
		state
	) => {
		let inner: ActionReturn<Options<Block>, DndZoneAttributes<Block>> | null = null;
		const sync = (next: typeof state) => {
			// The cursor, not the dragged block's centre, says where it is
			// going: a chart is wide, its handle is in a corner, and the
			// centre of a block held by its corner is a column away from
			// where the person is pointing.
			const options = {
				items: next.items,
				flipDurationMs,
				dropTargetStyle: {},
				useCursorForDetection: true
			};
			if (next.active && !inner) inner = dragHandleZone(node, options);
			else if (!next.active && inner) {
				inner.destroy?.();
				inner = null;
			} else inner?.update?.(options);
		};
		sync(state);
		return { update: sync, destroy: () => inner?.destroy?.() };
	};
</script>

<svelte:head><title>Dashboard · Tracepad</title></svelte:head>

<PageHeader title="Dashboard">
	{#snippet meta()}
		{#if lastTrace}
			<span class="whitespace-nowrap" title={timestamp(lastTrace.timestamp)}>
				Last trace {relative(lastTrace.timestamp)}
			</span>
		{:else if lastTrace === null}
			<span class="whitespace-nowrap">No traces yet</span>
		{/if}
		{#if loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button onclick={() => generation++} busy={loading} title="Read the statistics again">
			<RefreshCw class="size-4" />
			Refresh
		</Button>
		{#if customizing}
			<Button onclick={() => keep(null)} title="Show every block in the default order">Reset</Button>
			<Button variant="primary" onclick={() => (customizing = false)}>Done</Button>
		{:else if !fresh}
			<Button onclick={() => (customizing = true)} title="Hide and reorder the blocks">
				<SlidersHorizontal class="size-4" />
				Customize
			</Button>
		{/if}
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
			{#each TIMELINES as size (size)}
				<!-- Minutes are a day at most (spec 034 #15): past that the
				     button stays, disabled, and says why. -->
				{@const fits = size !== 'minute' || minutesFit(range, clock)}
				<Button
					variant={bucket === size ? 'primary' : 'default'}
					aria-pressed={bucket === size}
					disabled={!fits}
					title={fits ? undefined : 'By the minute for a window of 24 hours or less'}
					onclick={() => navigate({ group_by: size })}
				>
					{SIZE_LABELS[size]}
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
	{#if fresh}
		<div class="mx-auto max-w-lg pt-4">
			<OnboardingCard />
		</div>
	{:else}
		<div
			class="grid grid-cols-1 gap-3 lg:grid-cols-2"
			aria-label="Dashboard blocks"
			use:zone={{ active: customizing, items }}
			onconsider={(event) => (items = event.detail.items)}
			onfinalize={dropped}
		>
			{#each items as item (item.id)}
				{@const id = item.id}
				<!-- The wrapper is the drag zone's item: what the library moves,
				     flips and announces by its label. -->
				<div
					class={['min-w-0', BLOCKS.find((block) => block.id === id)?.wide && 'lg:col-span-2']}
					aria-label={label(id)}
					animate:flip={{ duration: flipDurationMs }}
				>
					<DashboardBlock label={label(id)} {customizing} onhide={() => hide(id)}>
						{#if id === 'summary'}
							<div class="grid grid-cols-2 gap-3 lg:grid-cols-4">
								{#each figures as figure (figure.id)}
									<SummaryTile {figure} loading={loading && summary === null} />
								{/each}
							</div>
						{:else if id === 'traces'}
							<Chart
								title="Traces"
								x={series.x}
								lines={[{ label: 'Traces', values: series.count, token: 'accent' }]}
								format={(value) => count(value)}
								summary="{count(sum(series.count))} traces across {series.x.length} {bucket} buckets."
							/>
						{:else if id === 'cost'}
							<Chart
								title="Cost"
								x={series.x}
								lines={[{ label: 'Cost', values: series.cost, token: 'ok' }]}
								format={(value) => cost(value)}
								summary="{cost(series.cost.some((value) => value !== null) ? sum(series.cost) : null)} in total; a bucket where nothing reported a cost is a gap, not a zero."
							/>
						{:else if id === 'tokens'}
							<Chart
								title="Tokens"
								x={series.x}
								lines={[
									{ label: 'Input', values: series.input, token: 'accent' },
									{ label: 'Output', values: series.output, token: 'ok' },
									{ label: 'Cache read', values: series.cacheRead, token: 'muted' },
									// Read off the legend, not drawn: reasoning may sit inside
									// output and would double-count beside it (spec 049 #9).
									{ label: 'Reasoning', values: series.reasoning, token: 'warn', hidden: true },
									{ label: 'Cache write', values: series.cacheWrite, token: 'subtle', hidden: true }
								]}
								format={(value) => count(value)}
								summary="Input, output and cache-read tokens per {bucket}, with reasoning and cache-write counts in the legend; a bucket where nothing reported usage is a gap, not a zero."
							/>
						{:else if id === 'latency'}
							<Chart
								title="Latency"
								x={series.x}
								lines={[
									{ label: 'p50', values: series.p50, token: 'accent' },
									{ label: 'p95', values: series.p95, token: 'warn' }
								]}
								format={(value) => axisDuration(value)}
								summary="Median and 95th percentile latency per {bucket}."
							/>
						{:else if id === 'errors'}
							<Chart
								title="Errors"
								x={series.x}
								lines={[{ label: 'Errors', values: series.errors, token: 'danger' }]}
								format={(value) => count(value)}
								summary="{count(sum(series.errors))} traces with a failed observation."
							/>
						{:else if id === 'models'}
							<BreakdownTable
								tokens
								title="By model"
								label="Model"
								unit={models?.unit ?? 'observation'}
								rows={breakdown((models?.buckets ?? []) as StatsBucket[])}
							/>
						{:else if id === 'environments'}
							<BreakdownTable
								tokens
								title="By environment"
								label="Environment"
								unit={environments?.unit ?? 'trace'}
								rows={breakdown((environments?.buckets ?? []) as StatsBucket[])}
							/>
						{:else if id === 'releases'}
							<BreakdownTable
								tokens
								title="By release"
								label="Release"
								unit={releases?.unit ?? 'trace'}
								rows={breakdown((releases?.buckets ?? []) as StatsBucket[], '(no release)')}
							/>
						{:else if id === 'quality'}
							<section class="border-border bg-surface min-w-0 rounded-lg border p-2">
								<h2 class="text-subtle px-1 py-0.5 text-xs font-medium">Quality</h2>
								{#if cards.length > 0}
									<QualityCards
										series={cards}
										{configs}
										window={{ from: range.from, to: range.to, bucket: scoreBucket, now: new Date() }}
										more={scores.length - cards.length + scoresOmitted}
									/>
								{:else}
									<p class="text-subtle px-1 py-6 text-center text-sm">No scores in this window</p>
								{/if}
							</section>
						{/if}
					</DashboardBlock>
				</div>
			{/each}
		</div>

		{#if customizing}
			<div
				class="border-border bg-surface mt-3 flex flex-wrap items-center gap-2 rounded-lg border px-3 py-2 text-sm"
				aria-label="Hidden blocks"
			>
				{#if arrangement.hidden.length === 0}
					<span class="text-subtle">Nothing hidden. Drag a block by its handle, or hide it.</span>
				{:else}
					<span class="text-subtle">Hidden:</span>
					{#each arrangement.hidden as id (id)}
						<Button variant="ghost" class="h-6 px-1.5 text-xs" onclick={() => show(id)}>
							<Eye class="size-3.5" aria-hidden="true" />
							Show {label(id)}
						</Button>
					{/each}
				{/if}
				{#if saveFailure}
					<span role="alert" class="text-danger flex items-center gap-1">
						<TriangleAlert class="size-3.5 shrink-0" />
						{saveFailure}
					</span>
				{/if}
			</div>
		{/if}
	{/if}
</div>
