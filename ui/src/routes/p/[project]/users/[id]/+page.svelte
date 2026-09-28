<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Maximize2 from '@lucide/svelte/icons/maximize-2';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		ApiError,
		api,
		type DryRun,
		type Erasure,
		type Stats,
		type User
	} from '$lib/api/client.svelte';
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
	import { rememberedRange, rememberRange } from '$lib/range.svelte';
	import UserSessionsTab from '$lib/components/users/UserSessionsTab.svelte';
	import UserTracesTab from '$lib/components/users/UserTracesTab.svelte';
	import {
		ERASE_WAIT_SECONDS,
		ErasureWatch,
		describe,
		ended,
		settle,
		unanswered
	} from '$lib/erasure.svelte';
	import { cost, count, duration, middleEllipsis, timestamp } from '$lib/format';
	import { href, project } from '$lib/project.svelte';

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
	const range = $derived(readRange(page.url.searchParams));
	/**
	 * The window this page opens on: the one this browser remembers (spec 034
	 * #7), else thirty days (spec 023 #9).
	 */
	const fallback = rememberedRange(openedAt) ?? presetRange('30d', openedAt);
	const viewed = $derived(range.from || range.to ? range : fallback);
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
	 * The erasure of this user under way, if any: the dialog's, or one the
	 * project's erasures name on the way in, so a reload still shows it (spec
	 * 047 #18, #27). The page stays while it runs, rather than leave as if the user
	 * were gone.
	 */
	const watch = new ErasureWatch();

	// An editor's page asks whether this user is being erased: the project's
	// erasures name their user while they run and only then (spec 047 #9,
	// #14), so the listing is the one indexed read that answers it (#27). A
	// viewer may not read it, and is offered no erasure.
	$effect(() => {
		const who = id;
		const current = project.id;
		if (!project.editor || !current) return;
		const controller = new AbortController();
		void untrack(() => findRunning(current, who, controller.signal));
		return () => {
			controller.abort();
			// The page is reused for the next user's id: what it followed
			// was this user's erasure, not that one's (spec 047 #28).
			watch.forget();
		};
	});

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
		rememberRange(picked, new Date());
		navigate({ from: picked.from ?? '', to: picked.to ?? '' });
	}

	/**
	 * The erasure is the server's own dry run and echo (spec 023 #10, spec 005
	 * #8), rendered by the card Settings uses — one card, one contract. On
	 * success there is no user left to be on, so the page leaves.
	 */
	/** Follows this user's erasure when one is under way. */
	async function findRunning(current: string, who: string, signal?: AbortSignal) {
		try {
			const { erasures } = await api.erasures(current, signal);
			const running = erasures.find((one) => one.user_id === who && !ended(one));
			if (running && !signal?.aborted) watch.follow(current, running);
		} catch {
			// The banner is a courtesy; the page is the user's data.
		}
	}

	async function erase(confirm?: string): Promise<DryRun | string> {
		const current = project.id;
		if (!current) throw new ApiError(0, 'there is no project on screen to erase from');
		let answer: DryRun | Erasure;
		try {
			answer = await api.eraseUserData(
				current,
				id,
				confirm,
				confirm === undefined ? undefined : ERASE_WAIT_SECONDS
			);
		} catch (cause) {
			// Accepted or not, this page follows it if it was (spec 047 #27).
			const sentence =
				confirm === undefined ? null : unanswered(cause, id, 'this page shows it if it did');
			if (sentence === null) throw cause;
			void findRunning(current, id);
			throw new ApiError(0, sentence);
		}
		if (answer.dry_run) return answer as DryRun;
		// Still running on the server: the page stays, and says how far it
		// is, rather than leaving as if the user were gone.
		return settle(current, id, answer as Erasure, watch);
	}

	const tabClass = (active: boolean) =>
		active
			? 'border-accent text-fg border-b-2 px-3 py-2 text-sm font-medium'
			: 'text-muted hover:text-fg border-b-2 border-transparent px-3 py-2 text-sm';
</script>

<svelte:head><title>{id} · Users · Tracepad</title></svelte:head>

<PageHeader title="User">
	{#snippet meta()}
		<a href={href('/users')} class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			Users
		</a>
		<span class="truncate font-mono" title={id}>{middleEllipsis(id, 40)}</span>
		<CopyButton text={id} label="Copy the user id" />
		{#if loading}<LoaderCircle class="size-3.5 animate-spin" />{/if}
	{/snippet}
	{#snippet actions()}
		<!-- The same card Settings carries, and the same rule: erasing a
		     user's data is an editor's route, so a viewer reads this page and
		     is offered nothing on it (spec 028 #15). -->
		{#if project.editor}
			<Button onclick={() => (erasing = !erasing)} aria-expanded={erasing}>
				<Trash2 class="size-4" />
				Erase data
			</Button>
		{/if}
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
			ondone={() => {
				if (!watch.running) goto(href('/users'));
			}}
		/>
	</div>
{/if}

{#if watch.current}
	<!-- An erasure of this user under way, or how it ended while the page was
	     open (spec 047 #13, #18). -->
	<p
		role="status"
		data-testid="erasure-progress"
		class={[
			'border-border flex items-center gap-2 border-b px-4 py-2 text-sm',
			watch.current.state === 'failed' ? 'text-danger bg-danger-soft' : 'bg-surface'
		]}
	>
		{#if watch.running}<LoaderCircle class="size-4 shrink-0 animate-spin" />{/if}
		{describe(watch.current, id)}
	</p>
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
				<a href={href(`/traces?user_id=${encodeURIComponent(id)}`)} class="text-accent hover:underline">
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
				href={href(`/${tab}?user_id=${encodeURIComponent(id)}`)}
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
