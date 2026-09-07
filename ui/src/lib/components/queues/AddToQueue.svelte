<script lang="ts">
	import ClipboardCheck from '@lucide/svelte/icons/clipboard-check';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { Popover } from 'bits-ui';
	import { ApiError, api, type AnnotationQueue, type QueueTarget } from '$lib/api/client.svelte';
	import { count } from '$lib/format';
	import { FROM_TRACES_CAP } from '$lib/queues';
	import type { TraceFilters } from '$lib/api/traces';
	import Button from '../Button.svelte';

	// *Add to queue* at the two surfaces the choice is made (spec 024 #13): the
	// reader at a trace or an observation, and the manager at a filtered
	// listing. One component, because the choice is the same one — which
	// review programme does this belong to — and only what is being added
	// differs.
	//
	// The queues are read when the popover opens rather than with the screen:
	// this is a control most readers never touch, and a listing that paid for
	// it on every trace would pay for it mostly for nothing.

	let {
		/** One target — a trace, or one observation of it. */
		target = undefined,
		/** Or the filters of a listing, with what they match (#13). */
		filters = undefined,
		matched = undefined,
		blocked = null,
		label = 'Add to queue',
		/**
		 * Show the icon alone. The observation panel's header already carries a
		 * name, an id and *Add to dataset*, and a third word there truncates
		 * the name of the thing being read to two characters.
		 */
		compact = false
	}: {
		target?: QueueTarget;
		filters?: TraceFilters;
		matched?: string;
		/** Why the filtered add may not run — above the cap, say. */
		blocked?: string | null;
		label?: string;
		compact?: boolean;
	} = $props();

	let open = $state(false);
	let queues = $state.raw<AnnotationQueue[] | null>(null);
	let picked = $state('');
	let busy = $state(false);
	let failure = $state<string | null>(null);
	let done = $state<string | null>(null);

	$effect(() => {
		if (!open) return;
		const controller = new AbortController();
		done = null;
		failure = null;
		api
			.listQueues(controller.signal)
			.then((listing) => {
				if (controller.signal.aborted) return;
				queues = listing.queues;
				if (picked === '') picked = listing.queues[0]?.name ?? '';
			})
			.catch((cause: unknown) => {
				if (controller.signal.aborted) return;
				failure = cause instanceof ApiError ? cause.message : 'Failed to read the queues.';
			});
		return () => controller.abort();
	});

	async function add() {
		busy = true;
		failure = null;
		done = null;
		try {
			if (filters) {
				const answer = await api.queueFromTraces(picked, filters, FROM_TRACES_CAP);
				done =
					`${count(answer.added)} added, ${count(answer.existing)} already there` +
					(answer.capped ? `, and more matched than the cap of ${count(FROM_TRACES_CAP)}` : '');
			} else if (target) {
				const answer = await api.addQueueItems(picked, target);
				done = answer.added > 0 ? `Added to ${picked}.` : `Already in ${picked}.`;
			}
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to add to the queue.';
		} finally {
			busy = false;
		}
	}
</script>

<Popover.Root bind:open>
	<Popover.Trigger>
		{#snippet child({ props })}
			<!-- The name is on the control rather than only in it: the word is
			     hidden at a phone's width, and a button whose accessible name
			     disappears with its text is a button nothing can address. -->
			<Button {...props} aria-label={label} title="Put this in a review queue">
				<ClipboardCheck class="size-4" />
				{#if !compact}
					<span class="hidden sm:inline">{label}</span>
				{/if}
			</Button>
		{/snippet}
	</Popover.Trigger>
	<Popover.Portal>
		<Popover.Content
			sideOffset={6}
			align="end"
			class="border-border bg-canvas shadow-overlay z-50 w-[min(22rem,calc(100vw-1.5rem))]
				rounded-lg border p-3"
		>
			{#if queues === null && !failure}
				<p class="text-subtle flex items-center gap-2 text-sm">
					<LoaderCircle class="size-4 animate-spin" />
					Reading the queues
				</p>
			{:else if queues && queues.length === 0}
				<p class="text-muted text-sm">
					This project has no review queues yet.
					<a class="text-accent underline underline-offset-2" href="/queues">Make one</a>.
				</p>
			{:else if queues}
				<label for="add-to-queue" class="mb-1 block text-xs font-medium">Queue</label>
				<select
					id="add-to-queue"
					name="queue"
					bind:value={picked}
					class="border-border bg-canvas text-fg w-full rounded-md border px-2 py-1 text-sm"
				>
					{#each queues as queue (queue.name)}
						<option value={queue.name}>{queue.name}</option>
					{/each}
				</select>
				{#if matched}
					<!-- The count before the call, so a filter matching ten
					     thousand is seen before the cap is hit (#13). -->
					<p class="text-muted mt-1.5 text-sm">
						{matched} match these filters; at most {count(FROM_TRACES_CAP)} are added, newest first.
					</p>
				{/if}
				{#if blocked}
					<p class="text-warn mt-1.5 text-sm">{blocked}</p>
				{/if}
				<Button
					class="mt-3"
					variant="primary"
					{busy}
					disabled={picked === '' || blocked !== null}
					onclick={add}
				>
					{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
					Add
				</Button>
			{/if}
			{#if failure}
				<p role="alert" class="text-danger mt-2 text-sm">{failure}</p>
			{:else if done}
				<p role="status" class="text-ok mt-2 text-sm">{done}</p>
			{/if}
		</Popover.Content>
	</Popover.Portal>
</Popover.Root>
