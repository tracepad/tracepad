<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Play from '@lucide/svelte/icons/play';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		ApiError,
		api,
		type AnnotationItem,
		type AnnotationQueue,
		type Trace
	} from '$lib/api/client.svelte';
	import { ITEM_STATUSES, readQueueItemFilters, queueItemSearch } from '$lib/api/queues';
	import { annotator } from '$lib/annotator.svelte';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PaginationBar from '$lib/components/PaginationBar.svelte';
	import PeekPanel from '$lib/components/PeekPanel.svelte';
	import DeleteQueueDialog from '$lib/components/queues/DeleteQueueDialog.svelte';
	import ProgressBar from '$lib/components/queues/ProgressBar.svelte';
	import QueueItemTable from '$lib/components/queues/QueueItemTable.svelte';
	import TraceDetail from '$lib/components/TraceDetail.svelte';
	import { asPage, Listing, UrlSpot, Walk } from '$lib/listing.svelte';
	import { freshSearch } from '$lib/page';
	import { peekSearch, readPeek } from '$lib/peek';

	// One queue (spec 024 #11): what is done, by whom, what was skipped and
	// why — and the two verbs a manager needs on a row. The peek is the trace,
	// because that is what the item points at; an item that names an
	// observation opens on it.

	const name = $derived(page.params.name ?? '');
	const filters = $derived(readQueueItemFilters(page.url.searchParams));

	let queue = $state.raw<AnnotationQueue | null>(null);
	let failure = $state<string | null>(null);

	$effect(() => {
		const controller = new AbortController();
		void load(name, controller.signal);
		return () => controller.abort();
	});

	/** Without a signal: a refresh after a write below, which nothing aborts. */
	async function load(wanted: string, signal?: AbortSignal) {
		failure = null;
		try {
			const answer = await api.getQueue(wanted, signal);
			if (!signal?.aborted) queue = answer;
		} catch (cause) {
			if (signal?.aborted) return;
			queue = null;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the queue.';
		}
	}

	const listing = new Listing<AnnotationItem>({
		key: () => `${name}|${queueItemSearch(filters)}`,
		spot: new UrlSpot(),
		read: async (at, counting, signal) => {
			const answer = await api.listQueueItems(name, filters, asPage(at, counting), signal);
			return { ...answer, rows: answer.items };
		},
		failed: 'Failed to read the items.'
	});

	const peekID = $derived(readPeek(page.url.searchParams).peek);
	const peeked = $derived(listing.rows.find((row) => row.id === peekID) ?? null);
	let peekedTrace = $state.raw<Trace | null>(null);

	const walk = new Walk(listing, {
		key: (row) => String(row.seq).padStart(12, '0'),
		peekID: () => peekID,
		showing: () => null,
		open: peek,
		ascending: true
	});

	/** Opening pushes one entry; moving between rows replaces it (spec 008 #6). */
	function peek(id: string | null) {
		const item = id === null ? null : listing.rows.find((row) => row.id === id);
		const search = peekSearch(page.url.searchParams, {
			peek: id,
			// The item names an observation, so the panel opens on it: what is
			// being judged is that step and not the whole run (#11).
			obs: item?.observation_id ?? null
		});
		goto(`${page.url.pathname}${search}`, {
			replaceState: id === null || peekID !== null,
			keepFocus: true,
			noScroll: true
		});
	}

	/** A status filter is a fresh listing: first page, same size. */
	function narrow(status: string) {
		const next = status === '' ? {} : { ...filters, status: status as never };
		goto(
			`/queues/${encodeURIComponent(name)}${freshSearch(queueItemSearch(next), page.url.searchParams)}`,
			{ keepFocus: true }
		);
	}

	let busyID = $state.raw<string | null>(null);
	let deleting = $state(false);

	/** A write on a row: the row moves, and so does the header's progress. */
	async function act(item: AnnotationItem, what: 'reopen' | 'remove') {
		busyID = item.id;
		try {
			if (what === 'reopen') {
				// The annotator is the signature on the act, and the desk has
				// already asked for one; a manager who has not annotated yet
				// signs as the interface.
				await api.reopenQueueItem(name, item.id, annotator.name ?? 'web');
			} else {
				await api.deleteQueueItem(name, item.id);
				if (peekID === item.id) peek(null);
			}
			listing.reload();
			await load(name);
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'The write failed.';
		} finally {
			busyID = null;
		}
	}
</script>

<svelte:head><title>{name} · Queues · Tracepad</title></svelte:head>

<PageHeader title={name}>
	{#snippet meta()}
		<a href="/queues" class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			Queues
		</a>
		{#if queue}
			<span class="hidden truncate md:inline" title={queue.description}>{queue.description}</span>
			<div class="hidden flex-wrap gap-1 sm:flex">
				{#each queue.score_configs as config (config)}
					<span
						class="border-border bg-surface text-muted rounded-md border px-1.5 py-0.5 text-xs"
					>
						{config}
					</span>
				{/each}
			</div>
			<ProgressBar {queue} />
		{/if}
	{/snippet}
	{#snippet actions()}
		{#if queue}
			<Button
				variant="primary"
				aria-label="Start annotating"
				disabled={queue.counts.pending === 0}
				title={queue.counts.pending === 0
					? 'Nothing is pending in this queue'
					: 'Work through the queue one item at a time'}
				onclick={() => goto(`/queues/${encodeURIComponent(name)}/annotate`)}
			>
				<Play class="size-4" />
				<span class="hidden sm:inline">Start annotating</span>
			</Button>
			<Button aria-label="Delete queue" onclick={() => (deleting = true)}>
				<Trash2 class="size-4" />
				<span class="hidden sm:inline">Delete</span>
			</Button>
		{/if}
	{/snippet}
</PageHeader>

<DeleteQueueDialog open={deleting} {name} onclose={() => (deleting = false)} />

{#if failure && !queue}
	<div class="flex flex-1 items-start justify-center p-8">
		<p role="alert" class="text-danger flex max-w-md items-start gap-2">
			<TriangleAlert class="mt-0.5 size-4 shrink-0" />
			{failure}
		</p>
	</div>
{:else if !queue}
	<div class="text-subtle flex flex-1 items-center justify-center gap-2">
		<LoaderCircle class="size-4 animate-spin" />
		Loading the queue
	</div>
{:else}
	<div class="border-border flex flex-wrap items-center gap-2 border-b px-4 py-2">
		<label class="text-subtle flex items-center gap-1.5 text-xs">
			Status
			<select
				name="status"
				value={filters.status ?? ''}
				onchange={(event) => narrow(event.currentTarget.value)}
				class="border-border bg-canvas text-fg rounded-md border px-1.5 py-1 text-sm"
			>
				<option value="">Any</option>
				{#each ITEM_STATUSES as status (status)}
					<option value={status}>{status}</option>
				{/each}
			</select>
		</label>
		{#if filters.annotator}
			<span class="text-subtle text-xs">by {filters.annotator}</span>
		{/if}
	</div>

	{#if listing.problem || failure}
		<p
			role="alert"
			class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
		>
			<TriangleAlert class="size-4 shrink-0" />
			{listing.problem ?? failure}
		</p>
	{/if}

	{#if listing.rows.length > 0 || !listing.newest}
		<QueueItemTable
			rows={listing.rows}
			onopen={peek}
			href={(id) => peekSearch(page.url.searchParams, { peek: id })}
			selectedID={peekID}
			{busyID}
			onreopen={(item) => act(item, 'reopen')}
			onremove={(item) => act(item, 'remove')}
		/>
		<PaginationBar {...listing.bar} noun="item" />
		{#if listing.rows.length === 0 && !listing.loading}
			<p class="text-subtle flex flex-1 items-start justify-center p-8 text-center">
				Nothing on this page any more. Use « to go back to the first.
			</p>
		{/if}
	{:else if !listing.loading && !listing.failure}
		<div class="flex flex-1 items-start justify-center overflow-auto p-8">
			<div class="max-w-lg">
				<h2 class="font-medium">
					{filters.status ? `Nothing is ${filters.status}` : 'This queue is empty'}
				</h2>
				<p class="text-muted mt-1">
					Add a trace from its own page, add a filtered listing from <a
						class="text-accent underline underline-offset-2"
						href="/traces">Traces</a
					>, or fill it from a script:
				</p>
				<div class="border-border bg-surface mt-3 flex items-start gap-2 rounded-md border p-3">
					<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{`tracepad queues add ${name} --from-traces --error --since 168h`}</pre>
					<CopyButton
						text={`tracepad queues add ${name} --from-traces --error --since 168h`}
						label="Copy the add command"
					/>
				</div>
			</div>
		</div>
	{:else}
		<div class="flex-1"></div>
	{/if}
{/if}

{#if peekID && peeked}
	<PeekPanel
		label="Trace"
		onclose={() => peek(null)}
		onprev={() => walk.step(-1)}
		onnext={() => walk.step(1)}
		hasPrev={walk.hasPrev}
		hasNext={walk.hasNext}
		fullHref="/traces/{encodeURIComponent(peeked.trace_id)}{peeked.observation_id
			? `?obs=${encodeURIComponent(peeked.observation_id)}`
			: ''}"
		fullLabel="Open this trace as a page"
	>
		{#snippet title()}
			<h2 class="truncate text-lg font-semibold tracking-tight">
				{peekedTrace?.name ?? 'Trace'}
			</h2>
		{/snippet}
		{#snippet meta()}
			<span class="shrink-0">#{peeked.seq}</span>
			<span class="shrink-0">{peeked.status}</span>
			<span class="hidden truncate font-mono lg:inline">{peeked.trace_id}</span>
			<CopyButton text={peeked.trace_id} label="Copy the trace id" />
		{/snippet}
		<TraceDetail traceID={peeked.trace_id} bind:trace={peekedTrace} />
	</PeekPanel>
{/if}
