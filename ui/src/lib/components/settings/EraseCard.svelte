<script lang="ts">
	import { api, type DryRun, type Erasure, type Project } from '$lib/api/client.svelte';
	import {
		ERASURE_POLL_MS,
		ErasureWatch,
		confirmErasure,
		describe,
		ended,
		stage
	} from '$lib/erasure.svelte';
	import { count, relative } from '$lib/format';
	import ConfirmCard from '../ConfirmCard.svelte';
	import Card from './Card.svelte';
	import ViewerNote from './ViewerNote.svelte';

	// Erasing one end user's data (spec 005 #7, spec 044 #1): the traces filed
	// under that user id with what hangs off them, the scores on their
	// sessions, the dataset items cut from them, and their spans inside the
	// raw archive. What the erasure cannot reach is the server's to say, in
	// the preview's note, rather than this screen's.
	//
	// The echo here is the user id, because the user is what is being erased.
	// A confirmed erasure is a task on the server (spec 047 #6): answered at
	// once when it is small, followed here while it runs, and listed below
	// with the project's others, so a reload still shows what is running
	// (#14, #18).

	let { current, readOnly = false }: { current: Project; readOnly?: boolean } = $props();

	let userID = $state('');
	const target = $derived(userID.trim());
	/** The user the followed erasure is of: the field may have moved on. */
	let erasing = $state('');
	/**
	 * Whether the erasure outlasted the dialog's wait, and so has a line of
	 * its own here; one that ended within it is the dialog's sentence.
	 */
	let following = $state(false);

	const watch = new ErasureWatch();
	let recent = $state.raw<Erasure[]>([]);

	async function erase(confirm?: string): Promise<DryRun | string> {
		const user = target;
		const project = projectID;
		if (confirm === undefined) return (await api.eraseUserData(project, user)) as DryRun;
		// Accepted or not, the listing below says (spec 047 #27); an answer
		// that comes back to another project's card is not this card's (#31).
		return confirmErasure({
			project,
			user,
			confirm,
			watch,
			where: 'the list below shows whether it did',
			still: () => project === projectID,
			lost: () => void list(),
			answered: (erasure) => {
				erasing = user;
				following = !ended(erasure);
				void list();
			}
		});
	}

	async function list() {
		const asked = projectID;
		try {
			const { erasures } = await api.erasures(asked);
			if (asked === projectID) recent = erasures;
		} catch {
			// The listing is what is on the side; the erasure itself is the
			// card's, and a listing that failed leaves the last one standing.
		}
	}

	// The listing, on the way in and while anything in it is still running;
	// and once more when the erasure this card follows ends. Another project
	// is another card's worth: what this one followed is forgotten (#29). It
	// follows the values, not the record, which is read again with the
	// account's projects.
	const projectID = $derived(current.id);
	const viewer = $derived(readOnly);
	$effect(() => {
		if (viewer) return;
		void projectID;
		void list();
		return () => {
			watch.forget();
			following = false;
			erasing = '';
			recent = [];
		};
	});
	// Not while the card follows its own erasure: that one is read every
	// tick already, and the listing is read again when it ends (#28).
	const listed = $derived(recent.some((one) => !ended(one)) && !watch.running);
	$effect(() => {
		if (!listed) return;
		const timer = setInterval(() => void list(), ERASURE_POLL_MS);
		return () => clearInterval(timer);
	});
	const followed = $derived(watch.current);
	$effect(() => {
		if (followed && ended(followed)) void list();
	});
</script>

<Card
	title="Danger zone"
	description="Irreversible operations on this project's data. Each one shows what it would remove
		before it removes anything."
>
	{#if readOnly}
		<ViewerNote what="there is nothing here you can run" />
	{:else}
		<ConfirmCard
			title="Erase everything about one user"
			description="Answers a deletion request: every trace filed under this user id, with its
				observations, payloads and scores, the scores on its sessions, the dataset items cut from
				it, and its spans in the raw archive."
			echoLabel="user id"
			previewLabel="Show what would go"
			executeLabel="Erase this user's data"
			subject={target}
			ready={target !== ''}
			preview={() => erase()}
			execute={(confirm) => erase(confirm) as Promise<string>}
		>
			<label for="erase-user" class="text-muted mb-1 block text-xs font-medium">User id</label>
			<input
				id="erase-user"
				type="text"
				bind:value={userID}
				placeholder="user-4821"
				autocomplete="off"
				spellcheck="false"
				class="border-border bg-canvas placeholder:text-subtle w-full max-w-sm rounded-md border
					px-2 py-1 font-mono text-sm"
			/>
		</ConfirmCard>

		{#if following && followed}
			<p
				role="status"
				class={['mt-3 text-sm', { 'text-muted': !ended(followed) }, {
					'text-ok': followed.state === 'done'
				}, { 'text-danger': followed.state === 'failed' }]}
				data-testid="erasure-progress"
			>
				{describe(followed, erasing)}
			</p>
		{/if}

		{#if recent.length > 0}
			<div class="mt-4">
				<h4 class="text-subtle text-xs font-medium">Erasures of the last 30 days</h4>
				<ul class="divide-border mt-1 divide-y text-sm" aria-label="Recent erasures">
					{#each recent as one (one.id)}
						<li class="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 py-1.5">
							<code class="font-mono text-xs" title={one.id}>{one.id.slice(0, 8)}</code>
							<span class={one.state === 'failed' ? 'text-danger' : ended(one) ? 'text-muted' : ''}>
								{stage(one)}
							</span>
							<span class="text-muted tabular-nums">
								{count(one.deleted.traces ?? 0)}
								{(one.deleted.traces ?? 0) === 1 ? 'trace' : 'traces'}
							</span>
							<!-- The record names the user only while the erasure runs
							     (spec 047 #9). -->
							{#if one.user_id}
								<span class="font-mono text-xs">{one.user_id}</span>
							{/if}
							<span class="text-subtle ml-auto text-xs">{relative(one.created_at)}</span>
							{#if one.error}
								<span class="text-danger basis-full text-xs">{one.error}</span>
							{/if}
						</li>
					{/each}
				</ul>
			</div>
		{/if}
	{/if}
</Card>
