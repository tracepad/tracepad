<script lang="ts">
	import CheckCheck from '@lucide/svelte/icons/check-check';
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		ApiError,
		api,
		type AnnotationItem,
		type AnnotationQueue,
		type ScoreConfig,
		type ScoreInput,
		type Trace
	} from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import DeskForm from '$lib/components/queues/DeskForm.svelte';
	import TraceDetail from '$lib/components/TraceDetail.svelte';
	import { count } from '$lib/format';
	import { auth } from '$lib/auth.svelte';
	import { href } from '$lib/project.svelte';
	import { progress } from '$lib/queues';
	import { Scores } from '$lib/scores.svelte';

	// The desk (spec 024 #12): read, judge, next, with nothing to navigate. The
	// trace on the left is the same `TraceDetail` every other screen shows, and
	// the form on the right is one control per score the queue asks for.
	//
	// Every step is one of the API's own calls — `next` claims, the scores go
	// through spec 003's endpoint, `complete` is the server checking the shape
	// was filled. The desk adds no verb of its own (spec 004 #1).

	const name = $derived(page.params.name ?? '');

	let queue = $state.raw<AnnotationQueue | null>(null);
	// Read with the queue rather than off the scores block beside it: the
	// configs are what the controls are *built* from, so the form cannot be
	// drawn before they land, and `loading` below is what says so.
	let configs = $state.raw<ScoreConfig[]>([]);
	let item = $state.raw<AnnotationItem | null>(null);
	let pending = $state(0);
	let peeked = $state.raw<Trace | null>(null);
	let loading = $state(true);
	let busy = $state(false);
	let failure = $state<string | null>(null);
	let missing = $state.raw<string[]>([]);

	// Who is reviewing is who is signed in (spec 048 #15): the server holds
	// the claim by the account, so the desk names nobody and asks nothing. The
	// queue's name is the one dependency; `start` runs untracked so that what
	// it reads does not restart it.
	$effect(() => {
		const wanted = name;
		const controller = new AbortController();
		untrack(() => void start(wanted, controller.signal));
		return () => controller.abort();
	});

	async function start(wanted: string, signal: AbortSignal) {
		loading = true;
		failure = null;
		try {
			const [declared, opened] = await Promise.all([
				api.listScoreConfigs(signal),
				api.getQueue(wanted, signal)
			]);
			configs = declared.configs;
			queue = opened;
			await take(signal);
		} catch (cause) {
			if (signal.aborted) return;
			failure = cause instanceof ApiError ? cause.message : 'Failed to open the queue.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	/**
	 * Asks for the next item and claims it. Resume-first on the server, so a
	 * reload lands on the same trace rather than a different one mid-verdict
	 * (#5) — and the id is written into the URL, which is what makes that
	 * visible.
	 */
	async function take(signal?: AbortSignal) {
		const answer = await api.nextQueueItem(name, signal);
		if (signal?.aborted) return;
		item = answer.item;
		pending = answer.pending;
		missing = [];
		// `obs` beside `item`, because that is where `TraceDetail` reads the
		// observation to open from: an item that names one is about that step
		// and not the whole run (#12). Without it the desk opened every item on
		// the tree, and the "an observation that is not in the trace" note had
		// no way to appear at all (found in review).
		const search = new URLSearchParams();
		if (answer.item) {
			search.set('item', answer.item.id);
			if (answer.item.observation_id) search.set('obs', answer.item.observation_id);
		}
		const query = search.toString();
		// `goto`, not shallow `replaceState`: every other screen turns a page
		// this way, and `TraceDetail` reads `obs` off `page.url` — which a
		// shallow replace leaves where it was, so the panel stayed on the
		// trace's first span however right the address bar looked.
		await goto(href(`/queues/${encodeURIComponent(name)}/annotate`, query), {
			replaceState: true,
			keepFocus: true,
			noScroll: true
		});
	}

	// The scores on the item's trace, read once beside it — the same read the
	// trace's own header makes, and the one the form prefills from (#12).
	const scores = new Scores(() => ({ trace_id: item?.trace_id ?? '' }));
	$effect(() => {
		if (!item) return;
		return untrack(() => scores.watch());
	});
	/**
	 * The scores of *this item's target*: a trace item takes the ones that
	 * name no observation, an observation item its own. The two are different
	 * verdicts about different things, which is the rule `complete` is checked
	 * by (#7).
	 */
	const onTarget = $derived(
		scores.rows.filter((score) => (score.observation_id ?? '') === (item?.observation_id ?? ''))
	);

	/** Every write the desk makes, with the busy state and the failure around it. */
	async function act(what: () => Promise<unknown>) {
		if (!item) return;
		busy = true;
		// Both, and for the same reason: what is on screen is about the
		// attempt that is starting. A `missing` left over from the last one
		// hides the banner (which is drawn only when there is no `missing`)
		// and goes on marking a control the server has stopped complaining
		// about — so a second attempt refused differently, or failing at the
		// network, told the reviewer the wrong thing (found in review).
		failure = null;
		missing = [];
		try {
			await what();
			await take();
			// The header's progress is a count on the queue, so it moves when
			// an item does.
			queue = await api.getQueue(name);
		} catch (cause) {
			if (cause instanceof ApiError && Array.isArray(cause.details.missing)) {
				// The server checked the stored scores and found the shape
				// unfilled: those controls are marked and nothing advances
				// (#7, #12).
				missing = cause.details.missing as string[];
				failure = cause.message;
				return;
			}
			failure = cause instanceof ApiError ? cause.message : 'The write failed.';
		} finally {
			busy = false;
		}
	}

	function complete(bodies: ScoreInput[]) {
		const current = item;
		if (!current) return;
		void act(async () => {
			// The scores that changed, then the completion: a queue over a
			// target somebody has already judged completes without a write.
			for (const body of bodies) await api.createScore(body);
			await api.completeQueueItem(name, current.id);
		});
	}

	function skip(reason: string) {
		const current = item;
		if (!current) return;
		void act(() => api.skipQueueItem(name, current.id, reason));
	}

	/** *Later*: the claim goes so nobody waits out its ten minutes (#16). */
	function later() {
		const current = item;
		if (!current) return;
		busy = true;
		api
			.reopenQueueItem(name, current.id)
			.catch(() => {
				// The claim expires on its own; leaving is what was asked for.
			})
			.finally(() => void goto(href(`/queues/${encodeURIComponent(name)}`)));
	}

	const at = $derived(queue ? progress(queue) : null);
</script>

<svelte:head><title>Annotating {name} · Tracepad</title></svelte:head>

<header class="border-border flex h-12 shrink-0 items-center gap-3 border-b px-4">
	<a
		href={href(`/queues/${encodeURIComponent(name)}`)}
		class="text-subtle hover:text-fg flex items-center gap-0.5 text-sm whitespace-nowrap"
	>
		<ChevronLeft class="size-3.5" />
		{name}
	</a>
	{#if at}
		<span class="text-subtle text-sm tabular-nums">{at.label}</span>
	{/if}
	{#if item}
		<!-- At every width: it is the one thing on the desk that says *which*
		     item is in front of you, and a reviewer coming back to a reloaded
		     tab reads it before anything else. -->
		<span class="text-subtle shrink-0 font-mono text-xs" title="Its place in the queue">
			#{item.seq}
		</span>
	{/if}
	<div class="ml-auto flex items-center gap-1.5">
		<!-- The account's own name (spec 048 #15): who is reviewing is who is
		     signed in, and changing it is signing in as somebody else. -->
		<span class="text-muted max-w-48 truncate text-sm" title="Reviewing as your account">
			{auth.displayName}
		</span>
	</div>
</header>

{#if failure && missing.length === 0}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{failure}
	</p>
{/if}

{#if loading}
	<div class="text-subtle flex flex-1 items-center justify-center gap-2">
		<LoaderCircle class="size-4 animate-spin" />
		Taking the next item
	</div>
{:else if item && queue}
	<!-- Side by side on a desktop, and the trace *over* the form on a phone
	     (Application contract) — where "over" means the desk scrolls: the trace
	     takes a screenful, the form follows under it, and neither is squeezed
	     into a third of a phone. Sideways it never scrolls at all.
	     `overflow-hidden` on the trace either way: it is a two-pane screen of
	     its own, and unclipped its blocks are drawn over the form. -->
	<div class="flex min-h-0 flex-1 flex-col overflow-y-auto md:flex-row md:overflow-hidden">
		<div
			class="border-border flex h-[60vh] shrink-0 flex-col overflow-hidden
				md:h-auto md:min-h-0 md:w-3/5 md:shrink md:border-r"
		>
			{#key item.id}
				<!-- A trace deleted from the desk takes its item with it
				     (spec 024 #3); the next item is what is left to do. -->
				<TraceDetail traceID={item.trace_id} bind:trace={peeked} ondeleted={() => void take()} />
			{/key}
		</div>
		<div
			class="border-border flex flex-col border-t
				md:min-h-0 md:w-2/5 md:overflow-hidden md:border-t-0"
		>
			<!-- Not before the target's scores have landed: the form is
			     *prefilled* from them (#12), and a form drawn empty and filled
			     in afterwards would overwrite whatever the reviewer had already
			     typed. Keyed on the item, so each one gets a fresh form. -->
			{#key item.id}
				{#if scores.loading}
					<p class="text-subtle flex items-center gap-2 p-4 text-sm">
						<LoaderCircle class="size-4 animate-spin" />
						Reading what has already been said about this trace
					</p>
				{:else}
					<DeskForm
						queue={name}
						{item}
						{configs}
						names={queue.score_configs}
						scores={onTarget}
						{missing}
						{busy}
						onsave={complete}
						onskip={skip}
						onlater={later}
					/>
				{/if}
			{/key}
		</div>
	</div>
{:else}
	<div class="flex flex-1 items-start justify-center overflow-auto p-8">
		<div class="max-w-lg">
			<h2 class="flex items-center gap-2 font-medium">
				<CheckCheck class="text-ok size-4" />
				{pending === 0 ? 'Nothing left in this queue' : 'Nothing to take right now'}
			</h2>
			<p class="text-muted mt-1">
				{#if pending === 0}
					Every item has been completed or skipped.
				{:else}
					{count(pending)} still pending, all of them claimed by somebody else. A claim lasts ten
					minutes, so come back.
				{/if}
			</p>
			<Button class="mt-3" onclick={() => goto(href(`/queues/${encodeURIComponent(name)}`))}>
				Back to {name}
			</Button>
		</div>
	</div>
{/if}
