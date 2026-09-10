<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { said } from '$lib/accounts';
	import { api, type AccountSession } from '$lib/api/client.svelte';
	import { timestamp } from '$lib/format';
	import Button from '../Button.svelte';
	import Card from './Card.svelte';

	// The list is the reason sessions are rows (spec 028 #8): "sign out
	// everywhere" is a decision somebody makes by looking at this and
	// recognising which one is the laptop they still have.
	//
	// The id shown is the row's — sha256 of the cookie — and cannot be turned
	// back into a credential, so it is safe to print and useless to steal.

	let sessions = $state.raw<AccountSession[]>([]);
	let loading = $state(true);
	let busy = $state(false);
	let failure = $state<string | null>(null);
	let notice = $state<string | null>(null);

	$effect(() => {
		const controller = new AbortController();
		void list(controller.signal);
		return () => controller.abort();
	});

	async function list(signal?: AbortSignal) {
		loading = true;
		try {
			sessions = (await api.listSignIns(signal)).sessions;
			failure = null;
		} catch (cause) {
			if (signal?.aborted) return;
			failure = said(cause, 'Failed to read the sessions.');
		} finally {
			if (!signal?.aborted) loading = false;
		}
	}

	async function endOthers() {
		busy = true;
		failure = null;
		notice = null;
		try {
			const { ended } = await api.endOtherSignIns();
			notice = ended === 1 ? 'One other session ended.' : `${ended} other sessions ended.`;
			await list();
		} catch (cause) {
			failure = said(cause, 'Failed to end the other sessions.');
		} finally {
			busy = false;
		}
	}
</script>

<Card
	title="Where you are signed in"
	description="One row per browser. A session lasts about a month and renews itself as you use it."
>
	{#if failure}
		<p role="alert" class="text-danger mb-3 text-sm">{failure}</p>
	{:else if notice}
		<p role="status" class="text-ok mb-3 text-sm">{notice}</p>
	{/if}

	{#if loading}
		<p class="text-subtle flex items-center gap-2 text-sm">
			<LoaderCircle class="size-4 animate-spin" />
			Reading the sessions
		</p>
	{:else}
		<ul class="border-border divide-border divide-y rounded-md border">
			{#each sessions as one (one.id)}
				<li class="flex flex-wrap items-baseline gap-x-3 gap-y-1 px-3 py-2">
					<span class="min-w-0 flex-1 truncate text-sm" title={one.user_agent}>
						{one.user_agent || 'an unnamed browser'}
					</span>
					<span class="text-subtle font-mono text-xs">{one.ip}</span>
					<span class="text-subtle text-xs tabular-nums">
						last seen {timestamp(one.last_seen_at)}
					</span>
					{#if one.current}
						<span class="text-ok text-xs font-medium">this browser</span>
					{/if}
				</li>
			{/each}
		</ul>

		<Button class="mt-3" onclick={endOthers} disabled={sessions.length < 2} {busy}>
			{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
			Sign out everywhere else
		</Button>
	{/if}
</Card>
