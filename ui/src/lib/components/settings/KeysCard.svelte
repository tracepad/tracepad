<script lang="ts">
	import KeyRound from '@lucide/svelte/icons/key-round';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Plus from '@lucide/svelte/icons/plus';
	import { said } from '$lib/accounts';
	import { api, type DryRun, type NewKey, type Project } from '$lib/api/client.svelte';
	import { timestamp } from '$lib/format';
	import Button from '../Button.svelte';
	import ConfirmCard from '../ConfirmCard.svelte';
	import SecretDialog from '../SecretDialog.svelte';
	import Card from './Card.svelte';
	import ViewerNote from './ViewerNote.svelte';

	// API keys (spec 007 #9). Several active pairs are what makes rotation
	// zero-downtime: mint, move the SDKs, revoke.
	//
	// The secret appears once, in the dialog, and the list shows public keys
	// only — because that is all the server has. Revoking asks twice: once here
	// and, when it is the project's last key, again with the echo the server
	// demands (spec 005 #12).

	let {
		current,
		/**
		 * A viewer sees neither the keys nor the buttons: listing them is an
		 * editor's route (Decision 3), so there is nothing to render read-only
		 * — only the line saying why.
		 */
		readOnly = false
	}: { current: Project; readOnly?: boolean } = $props();

	type Key = { public_key: string; created_at: string };

	let keys = $state.raw<Key[]>([]);
	let loading = $state(true);
	let minting = $state(false);
	let failure = $state<string | null>(null);
	let minted = $state.raw<NewKey | null>(null);
	/** The key a revocation is being walked through, if any. */
	let revoking = $state<string | null>(null);
	/**
	 * What just happened, kept here rather than inside the card: closing the
	 * card is what "done" means, and a message that lives in the thing being
	 * closed is a message nobody reads.
	 */
	let notice = $state<string | null>(null);

	$effect(() => {
		if (readOnly) return;
		const controller = new AbortController();
		list(controller.signal);
		return () => controller.abort();
	});

	async function list(signal?: AbortSignal) {
		loading = true;
		try {
			keys = (await api.listKeys(current.id, signal)).keys;
			failure = null;
		} catch (cause) {
			if (signal?.aborted) return;
			failure = said(cause, 'Failed to read the keys.');
		} finally {
			if (!signal?.aborted) loading = false;
		}
	}

	async function mint() {
		minting = true;
		failure = null;
		try {
			minted = await api.createKey(current.id);
			await list();
		} catch (cause) {
			failure = said(cause, 'Failed to mint a key.');
		} finally {
			minting = false;
		}
	}

	/**
	 * One revocation. Without `confirm` the server revokes an ordinary key
	 * outright and answers with a dry run for the last one — so the card
	 * behaves differently for the two cases without this screen having to
	 * decide which case it is in.
	 */
	async function revoke(publicKey: string, confirm?: string): Promise<DryRun | string> {
		const answer = await api.revokeKey(current.id, publicKey, confirm);
		if ('dry_run' in answer && answer.dry_run) return answer as DryRun;
		await list();
		notice = `Key ${publicKey} revoked.`;
		return notice;
	}
</script>

<Card
	title="API keys"
	description="Each pair authenticates ingest and every read. Rotate by minting a new one, moving
		your exporters over, then revoking the old."
>
	{#if readOnly}
		<ViewerNote what="the keys are not shown and cannot be rotated from here" />
	{:else}
		{#if failure}
			<p role="alert" class="text-danger mb-3 text-sm">{failure}</p>
		{:else if notice}
			<p role="status" class="text-ok mb-3 text-sm">{notice}</p>
		{/if}

		{#if loading}
			<p class="text-subtle flex items-center gap-2 text-sm">
				<LoaderCircle class="size-4 animate-spin" />
				Reading the keys
			</p>
		{:else}
			<ul class="border-border divide-border divide-y rounded-md border">
				{#each keys as key (key.public_key)}
					<li class="flex flex-wrap items-center gap-2 px-3 py-2">
						<KeyRound class="text-subtle size-4 shrink-0" />
						<span class="min-w-0 flex-1 truncate font-mono text-sm">{key.public_key}</span>
						<span class="text-subtle text-xs tabular-nums">{timestamp(key.created_at)}</span>
						<Button
							onclick={() => (
								(notice = null), (revoking = revoking === key.public_key ? null : key.public_key)
							)}
							aria-expanded={revoking === key.public_key}
						>
							Revoke
						</Button>
					</li>
				{/each}
			</ul>
		{/if}

		<Button class="mt-3" onclick={mint} busy={minting}>
			{#if minting}
				<LoaderCircle class="size-4 animate-spin" />
			{:else}
				<Plus class="size-4" />
			{/if}
			Mint a key pair
		</Button>

		{#if revoking}
			{@const publicKey = revoking}
			<div class="mt-3">
				<ConfirmCard
					title="Revoke {publicKey}"
					description="Anything still exporting with this pair stops being able to. If it is the
						project's last key, ingest stops until another is minted — and the server will ask for
						the project's name first."
					echoLabel="project name"
					previewLabel="Revoke"
					executeLabel="Revoke the last key"
					subject={publicKey}
					preview={() => revoke(publicKey)}
					execute={(confirm) => revoke(publicKey, confirm) as Promise<string>}
					ondone={() => (revoking = null)}
				/>
			</div>
		{/if}
	{/if}
</Card>

<SecretDialog pair={minted} onclose={() => (minted = null)} />
