<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Maximize2 from '@lucide/svelte/icons/maximize-2';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api, type DryRun, type Stats, type User } from '$lib/api/client.svelte';
	import { presetRange, readBucket, readRange, type Range } from '$lib/api/range';
	import { breakdown, buildSeries, type StatsBucket } from '$lib/api/stats';
	import { readTab, USER_TABS, userPageSearch } from '$lib/api/users';
	import BreakdownTable from '$lib/components/BreakdownTable.svelte';
	import Button from '$lib/components/Button.svelte';
	import Chart from '$lib/components/Chart.svelte';
	import ConfirmCard from '$lib/components/ConfirmCard.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import RangePicker from '$lib/components/RangePicker.svelte';
	import UserSessionsTab from '$lib/components/users/UserSessionsTab.svelte';
	import UserTracesTab from '$lib/components/users/UserTracesTab.svelte';
	import { cost, count, duration, middleEllipsis, timestamp } from '$lib/format';
	import { project } from '$lib/project.svelte';

	// One user (spec 023 #9). Every piece of it is a component the Stats and
	// listing screens already have: the summary is `GET /api/v1/users/{id}`,
	// the charts and the breakdowns are `GET /api/v1/stats?user_id=` through
	// the same `buildSeries` and `breakdown`, and the two tabs are the Traces
	// and Sessions tables under the shared loader. Nothing here computes a
	// number the API did not send.

	// Raw, as every other dynamic route in this app reads its param: SvelteKit
	// has already decoded it. Decoding a second time threw `URIError` on any id
	// carrying a bare `%` — inside a `$derived`, so the page did not render at
	// all — and quietly resolved an id containing `%2F` to a different one
	// (found in review of PR #42).
	const id = $derived(page.params.id ?? '');
	/**
	 * When this page was opened, read once. The default window is resolved
	 * against it rather than against the clock: a `$derived` reading `new
	 * Date()` produces a different `from` on every URL change, so switching
	 * tabs would re-ask for the charts with a window a few milliseconds
	 * narrower.
	 */
	const openedAt = new Date();
	/** Thirty days is the window this page opens on (spec 023 #9). */
	const range = $derived(readRange(page.url.searchParams));
	const viewed = $derived(range.from || range.to ? range : presetRange('30d', openedAt));
	const bucket = $derived(readBucket(page.url.searchParams, viewed, openedAt));
	const tab = $derived(readTab(page.url.searchParams));

	let user = $state.raw<User | null>(null);
	let timeline = $state.raw<StatsBucket[]>([]);
	let environments = $state.raw<Stats | null>(null);
	let models = $state.raw<Stats | null>(null);
	let loading = $state(true);
	let missing = $state(false);
	let failure = $state<string | null>(null);
	let erasing = $state(false);

	/**
	 * The question this page asks, as a value that compares. Not the objects
	 * behind it: `viewed` is rebuilt on every URL change, and a `$derived`
	 * object is never equal to the last one — so an effect that read it
	 * re-asked all four requests on `?peek=`, `?cursor=` and `?limit=`, every
	 * one of which belongs to the tab underneath rather than to the charts.
	 * Turning a page in the Traces tab fetched the summary, the timeline and
	 * both breakdowns again, aborting the last set each time.
	 *
	 * This is spec 010's own lesson (`listing.svelte.ts`, `#stamp`) in the one
	 * screen that carries two listings' state in its URL beside its own
	 * (found in the second review of PR #42).
	 */
	const asked = $derived(`${id}|${viewed.from ?? ''}|${viewed.to ?? ''}|${bucket}`);

	$effect(() => {
		// The stamp is the whole subscription; everything else is read
		// untracked, in this effect's own run and before the first await.
		void asked;
		const controller = new AbortController();
		untrack(() =>
			load(id, { user_id: id, from: viewed.from, to: viewed.to }, bucket, controller.signal)
		);
		return () => controller.abort();
	});

	async function load(
		who: string,
		query: { user_id: string; from?: string; to?: string },
		group: string,
		signal: AbortSignal
	) {
		loading = true;
		failure = null;
		missing = false;
		try {
			const [summary, series, byEnvironment, byModel] = await Promise.all([
				api.getUser(who, signal),
				api.getStats({ ...query, group_by: group }, signal),
				api.getStats({ ...query, group_by: 'environment' }, signal),
				api.getStats({ ...query, group_by: 'model' }, signal)
			]);
			if (signal.aborted) return;
			user = summary;
			timeline = series.buckets as StatsBucket[];
			environments = byEnvironment;
			models = byModel;
		} catch (cause) {
			if (signal.aborted) return;
			user = null;
			timeline = [];
			environments = models = null;
			// A 404 is not a failure to report — it is the answer, and it has
			// a state of its own with the id in it (spec 023, Application
			// contract).
			if (cause instanceof ApiError && cause.status === 404) missing = true;
			else failure = cause instanceof ApiError ? cause.message : 'Failed to read the user.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	const series = $derived(
		buildSeries(timeline, { from: viewed.from, to: viewed.to, bucket, now: openedAt })
	);
	/** The header's cards, as label/value pairs so one loop renders them. */
	const cards = $derived(
		user
			? [
					{ label: 'Traces', value: count(user.traces) },
					{ label: 'Sessions', value: count(user.sessions) },
					{ label: 'With errors', value: count(user.error_count) },
					{ label: 'Cost', value: cost(user.total_cost) },
					{ label: 'p50', value: duration(user.latency_ms?.p50) },
					{ label: 'p95', value: duration(user.latency_ms?.p95) },
					{ label: 'First seen', value: timestamp(user.first_seen) },
					{ label: 'Last seen', value: timestamp(user.last_seen) }
				]
			: []
	);

	/** The window and the tab live in the URL, so the page is a link. */
	function navigate(next: Partial<{ from: string; to: string; tab: string }>) {
		goto(`${page.url.pathname}${userPageSearch(page.url.searchParams, next)}`, {
			keepFocus: true,
			noScroll: true
		});
	}

	function setRange(picked: Range) {
		navigate({ from: picked.from ?? '', to: picked.to ?? '' });
	}

	/**
	 * The erasure is the server's own dry run and echo (spec 023 #10, spec 005
	 * #8), rendered by the card Settings uses — one card, one contract. On
	 * success there is no user left to be on, so the page leaves.
	 */
	async function erase(confirm?: string): Promise<DryRun | string> {
		const current = project.current;
		if (!current) throw new ApiError(0, 'the project has not loaded yet');
		const answer = await api.eraseUserData(current.id, id, confirm);
		if ('dry_run' in answer && answer.dry_run) return answer as DryRun;
		const deleted = (answer as { deleted: Record<string, number> }).deleted;
		return `Erased ${deleted.traces ?? 0} traces belonging to ${id}.`;
	}

	const tabClass = (active: boolean) =>
		active
			? 'border-accent text-fg border-b-2 px-3 py-2 text-sm font-medium'
			: 'text-muted hover:text-fg border-b-2 border-transparent px-3 py-2 text-sm';
</script>

<svelte:head><title>{id} · Users · Tracepad</title></svelte:head>

<PageHeader title="User">
	{#snippet meta()}
		<a href="/users" class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			Users
		</a>
		<span class="truncate font-mono" title={id}>{middleEllipsis(id, 40)}</span>
		<CopyButton text={id} label="Copy the user id" />
		{#if loading}<LoaderCircle class="size-3.5 animate-spin" />{/if}
	{/snippet}
	{#snippet actions()}
		<Button onclick={() => (erasing = !erasing)} aria-expanded={erasing}>
			<Trash2 class="size-4" />
			Erase data
		</Button>
	{/snippet}
</PageHeader>

{#if erasing}
	<div class="border-border border-b p-4">
		<ConfirmCard
			title="Erase everything about this user"
			description="Answers a deletion request: every trace filed under this user id, with its
				observations, payloads and scores, and the per-user statistics derived from them."
			echoLabel="user id"
			previewLabel="Show what would go"
			executeLabel="Erase this user’s data"
			subject={id}
			preview={() => erase()}
			execute={(confirm) => erase(confirm) as Promise<string>}
			ondone={() => goto('/users')}
		/>
	</div>
{/if}

{#if failure}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{failure}
	</p>
{/if}

{#if missing}
	<div class="flex flex-1 items-start justify-center overflow-auto p-8">
		<div class="max-w-lg">
			<h2 class="font-medium">Nothing is filed under <code class="font-mono">{id}</code></h2>
			<p class="text-muted mt-1">
				No trace this server holds names that user id — neither in the roll-up nor in the live
				traffic behind it. The
				<a href="/traces?user_id={encodeURIComponent(id)}" class="text-accent hover:underline">
					traces filtered by it
				</a>
				is the same question asked of the raw rows.
			</p>
		</div>
	</div>
{:else}
	{#if cards.length > 0}
		<dl class="border-border flex shrink-0 flex-wrap gap-x-8 gap-y-2 border-b px-4 py-3">
			{#each cards as card (card.label)}
				<div>
					<dt class="text-subtle text-xs">{card.label}</dt>
					<dd class="tabular-nums">{card.value}</dd>
				</div>
			{/each}
		</dl>
	{/if}

	<div class="min-h-0 flex-1 overflow-auto">
		<div class="border-border flex flex-wrap items-center gap-1.5 border-b px-4 py-2">
			<RangePicker range={viewed} onchange={setRange} />
		</div>

		<div class="grid grid-cols-1 gap-3 p-4 lg:grid-cols-2">
			<Chart
				title="Activity"
				x={series.x}
				lines={[
					{ label: 'Traces', values: series.count, token: 'accent' },
					{ label: 'Sessions', values: series.sessions, token: 'ok' },
					{ label: 'Errors', values: series.errors, token: 'danger' }
				]}
				format={(value) => count(value)}
				summary="Traces, sessions started and failing traces per {bucket} for this user."
			/>
			<Chart
				title="Cost"
				x={series.x}
				lines={[{ label: 'Cost', values: series.cost, token: 'ok' }]}
				format={(value) => cost(value)}
				summary="What this user cost per {bucket}; a bucket where nothing reported a cost is a gap, not a zero."
			/>
		</div>

		<div class="grid grid-cols-1 gap-3 px-4 pb-4 lg:grid-cols-2">
			<BreakdownTable
				title="By environment"
				label="Environment"
				unit={environments?.unit ?? 'trace'}
				rows={breakdown((environments?.buckets ?? []) as StatsBucket[])}
			/>
			<BreakdownTable
				title="By model"
				label="Model"
				unit={models?.unit ?? 'observation'}
				rows={breakdown((models?.buckets ?? []) as StatsBucket[])}
			/>
		</div>

		<div class="border-border flex items-center gap-1 border-y px-2">
			{#each USER_TABS as one (one)}
				<button
					type="button"
					onclick={() => navigate({ tab: one })}
					aria-pressed={tab === one}
					class="{tabClass(tab === one)} cursor-pointer capitalize transition-colors duration-100"
				>
					{one}
				</button>
			{/each}
			<a
				href="/{tab}?user_id={encodeURIComponent(id)}"
				title="Open the full {tab} listing filtered by this user"
				class="text-subtle hover:bg-raised hover:text-fg ml-auto inline-flex size-7 items-center
					justify-center rounded-md transition-colors duration-100"
			>
				<Maximize2 class="size-4" />
			</a>
		</div>

		<!-- Keyed so that switching tabs unmounts one listing and mounts the
		     other: two loaders live at once would both read on every URL
		     change, and only one of them is on screen. -->
		{#key tab}
			{#if tab === 'traces'}
				<UserTracesTab userID={id} />
			{:else}
				<UserSessionsTab userID={id} />
			{/if}
		{/key}
	</div>
{/if}
