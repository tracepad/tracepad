<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		ApiError,
		api,
		type Run,
		type RunItem,
		type RunWithSummary,
		type Trace
	} from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import RunItemDetail from '$lib/components/evals/RunItemDetail.svelte';
	import StatusChip from '$lib/components/evals/StatusChip.svelte';
	import Summary from '$lib/components/evals/Summary.svelte';
	import ListingShell from '$lib/components/ListingShell.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PeekPanel from '$lib/components/PeekPanel.svelte';
	import TraceDetail from '$lib/components/TraceDetail.svelte';
	import TracePeekMeta from '$lib/components/TracePeekMeta.svelte';
	import { compareHref, NO_ITEM, runItemKey, scoreText, short } from '$lib/evals';
	import { count, timestamp } from '$lib/format';
	import { asPage, Listing, UrlSpot, Walk } from '$lib/listing.svelte';
	import { freshSearch } from '$lib/page';
	import { project } from '$lib/project.svelte';
	import { peekSearch, readPeek } from '$lib/peek';
	import { href } from '$lib/project.svelte';

	// One run (spec 016, Application contract): the header, the summary the
	// server computed (spec 014 #11, #12), and its items with the attempts the
	// run made at each. While the run is `running` the page polls at the trace
	// listing's cadence and stops when the harness closes it (#7): the store
	// never guesses completion, so the screen shows progress as it lands.

	const POLL_MS = 5000;

	const id = $derived(page.params.id ?? '');
	const unknown = $derived(page.url.searchParams.get('unknown') === 'true');

	let run = $state.raw<RunWithSummary | null>(null);
	let failure = $state<string | null>(null);
	/**
	 * A poll's own failure slot, the loader's `liveFailure` one layer up
	 * (spec 010 #5): a run being watched must not become an error page because
	 * one re-read of it answered badly, and the rows on screen are still true.
	 */
	let liveFailure = $state<string | null>(null);
	/** The instant the ages are measured from; moved by every poll. */
	let now = $state(Date.now());
	/** Whether a poll is still out, so two of them never overlap (spec 010 #9). */
	let polling = false;

	$effect(() => {
		const controller = new AbortController();
		void load(id, controller.signal);
		return () => controller.abort();
	});

	async function load(wanted: string, signal: AbortSignal) {
		failure = liveFailure = null;
		try {
			const answer = await api.getRun(wanted, signal);
			if (signal.aborted) return;
			run = answer;
			now = Date.now();
		} catch (cause) {
			if (signal.aborted) return;
			run = null;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the run.';
		}
	}

	/**
	 * One poll: the run again, silently. It replaces nothing on a failure and
	 * reports in its own slot, so a transient answer cannot tear down the page
	 * — and with it the timer that would have recovered from it (found in
	 * review of this PR).
	 */
	async function poll(wanted: string, signal: AbortSignal) {
		if (polling) return;
		polling = true;
		try {
			const answer = await api.getRun(wanted, signal);
			if (signal.aborted) return;
			run = answer;
			now = Date.now();
			liveFailure = null;
		} catch (cause) {
			if (signal.aborted) return;
			liveFailure = cause instanceof ApiError ? cause.message : 'Failed to re-read the run.';
		} finally {
			polling = false;
		}
	}

	type Row = RunItem & { id: string };
	const listing = new Listing<Row>({
		key: () => `${id}|${unknown}`,
		spot: new UrlSpot(),
		count: false,
		read: async (at, counting, signal) => {
			const answer = await api.listRunItems(id, unknown, asPage(at, counting), signal);
			// The group of traces that named no item has a null id on the wire
			// (spec 014 #29); a row on a page and in a URL needs a word for it.
			return { ...answer, rows: answer.items.map((row) => ({ ...row, id: row.id ?? NO_ITEM })) };
		},
		failed: 'Failed to read the items.'
	});

	// Polling (#7): the summary and the newest page, silently, while the run
	// is open and the tab is looked at — the trace listing's own gate.
	//
	// Depends on the *status* rather than on the run: a poll assigns a new run
	// object every five seconds, and an effect that re-ran on it would rebuild
	// the timer each time and restart its phase. It stops when the harness
	// closes the run, and the controller it owns is what aborts a poll still
	// out when the id changes underneath it.
	const running = $derived(run?.status === 'running');
	$effect(() => {
		if (!running) return;
		const wanted = id;
		const controller = new AbortController();
		const timer = setInterval(() => {
			if (document.hidden) return;
			void poll(wanted, controller.signal);
			void listing.tick();
		}, POLL_MS);
		return () => {
			clearInterval(timer);
			controller.abort();
		};
	});

	// The peek panel (spec 008): a case beside the listing, and an attempt's
	// trace one level deeper in the same panel (#12, the session panel's
	// pattern).
	const opened = $derived(readPeek(page.url.searchParams));
	const peekID = $derived(opened.peek);
	const drilled = $derived(opened.trace);
	const selectedObs = $derived(page.url.searchParams.get('obs'));
	let peekedTrace = $state.raw<Trace | null>(null);
	const peeked = $derived(listing.rows.find((row) => row.id === peekID) ?? null);

	const walk = new Walk(listing, {
		key: runItemKey,
		peekID: () => peekID,
		showing: () => null,
		open: (item) => peek(item),
		ascending: true
	});

	/** Deepening pushes one history entry; moving sideways or up replaces (spec 008 #6). */
	function move(next: { peek: string | null; trace?: string | null }, deeper: boolean) {
		const search = peekSearch(page.url.searchParams, next);
		goto(`${page.url.pathname}${search}`, { replaceState: !deeper, keepFocus: true, noScroll: true });
	}
	const peek = (item: string | null) => move({ peek: item }, item !== null && peekID === null);
	const drill = (traceID: string | null) => move({ peek: peekID, trace: traceID }, traceID !== null);

	/** The unknown toggle is a fresh listing at the same size. */
	function showUnknown(on: boolean) {
		goto(`${page.url.pathname}${freshSearch(on ? '?unknown=true' : '', page.url.searchParams)}`, {
			keepFocus: true
		});
	}

	// Compare with… (#11): the dataset's other runs, newest first, in a select.
	let others = $state.raw<Run[]>([]);
	$effect(() => {
		const dataset = run?.dataset;
		if (!dataset) return;
		const controller = new AbortController();
		api
			.listDatasetRuns(dataset, { limit: 500 }, controller.signal)
			.then((answer) => {
				if (!controller.signal.aborted) others = answer.runs.filter((row) => row.id !== id);
			})
			.catch(() => {
				// The select stays empty; the page beside it is unaffected.
			});
		return () => controller.abort();
	});

	/** An item's value per score name, for the row: every attempt's, in order (spec 016 #17). */
	function values(row: RunItem): [string, string][] {
		const byName = new Map<string, string[]>();
		for (const attempt of row.attempts) {
			for (const score of attempt.scores) {
				const list = byName.get(score.name) ?? [];
				list.push(scoreText(score.value ?? score.string_value));
				byName.set(score.name, list);
			}
		}
		return [...byName.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([name, list]) => [name, list.join(', ')]);
	}

	// What a harness stamps to fill this run (#15): shown while nothing has
	// arrived, because the person on an empty run is the one writing it.
	const stamp = $derived(
		`tracepad.run_id  = ${id}\ntracepad.item_id = <item id>\n\n` +
			`curl -H "Authorization: Bearer <your project key>" ${page.url.origin}/api/v1/runs/${id}/finish -d '{}'`
	);
	// Deleting a run is one row of bookkeeping (spec 014 #20): the traces it
	// held are not deleted, they stop being pinned — which is the consequence
	// the dialog names (#6), because it is the one a reader cannot see from
	// here.
	let deleting = $state(false);

	const fieldClass = 'border-border bg-canvas text-fg rounded-md border px-2 py-1 text-sm';
</script>

<svelte:head><title>{run?.name ?? 'Run'} · Runs · Tracepad</title></svelte:head>

<PageHeader title={run?.name ?? (run ? short(run.id) : 'Run')}>
	{#snippet meta()}
		<a href={href('/runs')} class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			Runs
		</a>
		{#if run}
			<!-- The version links to the items tab at that version, read-only
			     when it is no longer the head (edge cases). -->
			<a
				class="hover:text-fg whitespace-nowrap"
				href={href(`/datasets/${encodeURIComponent(run.dataset)}?version=${run.dataset_version}`)}
			>
				{run.dataset}@{run.dataset_version}
			</a>
			<StatusChip status={run.status} since={run.created_at} {now} />
			<span class="hidden font-mono whitespace-nowrap sm:inline">{timestamp(run.created_at)}</span>
			{#if run.finished_at}
				<span class="hidden font-mono whitespace-nowrap md:inline">→ {timestamp(run.finished_at)}</span>
			{/if}
			<span class="hidden truncate font-mono lg:inline">{run.id}</span>
			<CopyButton text={run.id} label="Copy the run id" />
		{/if}
	{/snippet}
	{#snippet actions()}
		{#if run}
			<label class="text-subtle flex items-center gap-1.5 text-xs whitespace-nowrap">
				Compare with
				<select
					id="compare-with"
					name="compare-with"
					class={fieldClass}
					aria-label="Compare with another run"
					value=""
					onchange={(event) => {
						if (event.currentTarget.value) goto(href(compareHref(id, event.currentTarget.value)));
					}}
				>
					<option value="">{others.length === 0 ? 'no other run' : '…'}</option>
					{#each others as other (other.id)}
						<option value={other.id}>
							{other.name ?? short(other.id)} · v{other.dataset_version} · {other.status}
						</option>
					{/each}
				</select>
			</label>
			<!-- Reading and comparing runs is every role's; deleting one is an
			     editor's (spec 028 #15). -->
			{#if project.editor}
				<Button aria-label="Delete run" onclick={() => (deleting = true)}>
					<Trash2 class="size-4" />
					<span class="hidden sm:inline">Delete</span>
				</Button>
			{/if}
		{/if}
	{/snippet}
</PageHeader>

<ConfirmDialog
	open={deleting}
	title="Delete this run?"
	description="The run and its bookkeeping go. Its traces are not deleted — they stop being pinned
		and return to the retention window, so they live as long as retention says."
	confirmLabel="Delete the run"
	onconfirm={async () => {
		await api.deleteRun(id);
		await goto(href('/runs'));
	}}
	onclose={() => (deleting = false)}
/>

{#if failure}
	<div class="flex flex-1 items-start justify-center p-8">
		<p role="alert" class="text-danger flex max-w-md items-start gap-2">
			<TriangleAlert class="mt-0.5 size-4 shrink-0" />
			{failure}
		</p>
	</div>
{:else if !run}
	<div class="text-subtle flex flex-1 items-center justify-center gap-2">
		<LoaderCircle class="size-4 animate-spin" />
		Loading the run
	</div>
{:else}
	<div class="flex min-h-0 flex-1 flex-col overflow-auto">
		{#if run.error}
			<p role="alert" class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2">
				<TriangleAlert class="size-4 shrink-0" />
				{run.error}
			</p>
		{/if}
		{#if liveFailure}
			<!-- A poll that failed: what is on screen is the last good answer,
			     and the next tick is what recovers from this. -->
			<p role="alert" class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2">
				<TriangleAlert class="size-4 shrink-0" />
				{liveFailure}
			</p>
		{/if}

		<Summary summary={run.summary} metadata={run.metadata} />

		{#if run.summary.traces.count === 0}
			<div class="border-border border-b px-4 py-3">
				<h2 class="font-medium">No trace has named this run yet</h2>
				<p class="text-muted mt-1 text-sm">
					Stamp two attributes on the traces the harness exports, one per case, then close the run:
				</p>
				<div class="border-border bg-surface mt-2 flex items-start gap-2 rounded-md border p-3">
					<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{stamp}</pre>
					<CopyButton text={() => stamp} label="Copy the attributes" />
				</div>
			</div>
		{/if}

		<div class="border-border flex shrink-0 items-center gap-3 border-b px-4 py-2">
			<h2 class="text-sm font-medium">Items</h2>
			<span class="text-subtle text-xs tabular-nums">
				{count(run.summary.items.covered)} of {count(run.summary.items.total)} covered
			</span>
			<Button
				class="ml-auto"
				variant={unknown ? 'primary' : 'default'}
				aria-pressed={unknown}
				onclick={() => showUnknown(!unknown)}
				title="Also list the run's traces that name a case the dataset does not have at this version"
			>
				Unknown ({count(run.summary.items.unknown)})
			</Button>
		</div>

		<ListingShell {listing} noun="item" back="first">
			{#snippet table()}
				<div class="min-h-0 shrink-0 overflow-x-auto">
					<table aria-label="Items" class="w-full min-w-2xl table-fixed border-collapse text-left">
						<thead class="bg-canvas text-subtle text-xs whitespace-nowrap">
							<tr class="border-border border-b">
								<th scope="col" class="w-14 px-3 py-2 text-right font-medium">#</th>
								<th scope="col" class="w-28 px-3 py-2 font-medium">Item</th>
								<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Attempts</th>
								<th scope="col" class="px-3 py-2 font-medium">Scores</th>
							</tr>
						</thead>
						<tbody>
							{#each listing.rows as row (row.id)}
								{@const lit = row.id === peekID}
								<tr
									class={[
										'border-border hover:bg-raised border-b transition-colors duration-100',
										lit && 'bg-accent-soft'
									]}
								>
									<td class="text-muted px-3 py-1.5 text-right tabular-nums">{row.seq ?? '—'}</td>
									<td class="truncate px-3 py-1.5 font-mono text-xs">
										<!-- Enter opens the panel, as a click does; ⌘-click gets a
										     link to this same view with the case open. -->
										<a
											href={peekSearch(page.url.searchParams, { peek: row.id })}
											aria-current={lit ? 'true' : undefined}
											title={row.id}
											onclick={(event) => {
												if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
												event.preventDefault();
												peek(row.id);
											}}
										>
											{row.id === NO_ITEM ? 'no item' : short(row.id)}
										</a>
										{#if row.unknown}<span class="text-warn ml-1">unknown</span>{/if}
									</td>
									<td class="text-muted px-3 py-1.5 text-right tabular-nums">{row.attempts.length}</td>
									<td class="px-3 py-1.5">
										<ul class="flex flex-wrap gap-1.5">
											{#each values(row) as [name, value] (name)}
												<li class="border-border bg-surface rounded border px-1.5 py-0.5 text-xs">
													<span class="text-muted">{name}</span>
													<span class="font-medium tabular-nums">{value}</span>
												</li>
											{/each}
										</ul>
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			{/snippet}
			{#snippet empty()}
				<p class="text-subtle p-8 text-center">— This run's dataset version holds no items.</p>
			{/snippet}
		</ListingShell>
	</div>
{/if}

{#if peekID && run}
	<PeekPanel
		label={drilled ? 'Trace' : 'Item'}
		onclose={() => peek(null)}
		onprev={drilled ? undefined : () => walk.step(-1)}
		onnext={drilled ? undefined : () => walk.step(1)}
		hasPrev={walk.hasPrev}
		hasNext={walk.hasNext}
		fullHref={href(
			drilled
				? `/traces/${encodeURIComponent(drilled)}${selectedObs ? `?obs=${encodeURIComponent(selectedObs)}` : ''}`
				: `/datasets/${encodeURIComponent(run.dataset)}?version=${run.dataset_version}&peek=${peekID}`
		)}
		fullLabel={drilled ? 'Open this trace as a page' : 'Open this item in its dataset'}
	>
		{#snippet title()}
			{#if drilled}
				<!-- The way back up: this layer replaced the item, so the
				     breadcrumb is what returns to it (#12). -->
				<button
					type="button"
					onclick={() => drill(null)}
					class="text-muted hover:text-fg pointer-coarse:min-h-11 flex shrink-0 cursor-pointer
						items-center gap-0.5 whitespace-nowrap transition-colors duration-100"
				>
					<ChevronLeft class="size-3.5" />
					Item
				</button>
				<h2 class="truncate text-lg font-semibold tracking-tight">{peekedTrace?.name ?? 'Trace'}</h2>
			{:else}
				<h2 class="shrink-0 text-lg font-semibold tracking-tight">
					{peekID === NO_ITEM ? 'No item' : `Item ${short(peekID)}`}
				</h2>
			{/if}
		{/snippet}
		{#snippet meta()}
			{#if drilled}
				<!-- No release and no id: the attempt is one of this case's, and
				     what names it here is the item above, not the trace. -->
				<TracePeekMeta trace={peekedTrace} hide={['release', 'id', 'copy']} />
			{:else if peekID !== NO_ITEM}
				<span class="hidden truncate font-mono md:inline">{peekID}</span>
				<CopyButton text={peekID} label="Copy the item id" />
			{/if}
		{/snippet}

		<!-- Hidden rather than unmounted under a drilled trace, so the way back
		     costs nothing (the session panel's reasoning). -->
		<div class={drilled ? 'hidden' : 'flex min-h-0 flex-1 flex-col'}>
			<RunItemDetail item={peeked} dataset={run.dataset} version={run.dataset_version} ondrill={drill} />
		</div>
		{#if drilled}
			<TraceDetail traceID={drilled} bind:trace={peekedTrace} />
		{/if}
	</PeekPanel>
{/if}
