<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Plus from '@lucide/svelte/icons/plus';
	import { said } from '$lib/accounts';
	import { api, type DryRun, type Key, type NewKey, type Project } from '$lib/api/client.svelte';
	import { timestamp } from '$lib/format';
	import { minter, outlived } from '$lib/keys';
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
	//
	// Each row says which program holds the key, who minted it and whether it
	// is still in use (spec 045 #14): the three things to know before revoking
	// it. A key whose minter can no longer manage keys here says so on the
	// row, because nothing revoked it when they lost access (#10).

	let {
		current,
		/**
		 * A viewer sees neither the keys nor the buttons: listing them is an
		 * editor's route (Decision 3), so there is nothing to render read-only
		 * — only the line saying why.
		 */
		readOnly = false
	}: { current: Project; readOnly?: boolean } = $props();

	let keys = $state.raw<Key[]>([]);
	let loading = $state(true);
	let minting = $state(false);
	/** What the next key will be called: which program is going to hold it. */
	let name = $state('');
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
			minted = await api.createKey(current.id, name.trim());
			name = '';
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
		your exporters over, then revoking the old: the old one's last use says when nothing holds it
		any more."
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
			<div class="border-border overflow-x-auto rounded-md border">
				<table class="w-full border-collapse text-left text-sm">
					<thead class="text-subtle text-xs whitespace-nowrap">
						<tr class="border-border border-b">
							<th scope="col" class="px-3 py-1.5 font-medium">Name</th>
							<th scope="col" class="px-3 py-1.5 font-medium">Created</th>
							<th scope="col" class="px-3 py-1.5 font-medium">Last used</th>
							<th scope="col" class="w-24 px-3 py-1.5"><span class="sr-only">Actions</span></th>
						</tr>
					</thead>
					<tbody>
						{#each keys as key (key.public_key)}
							<tr class="border-border border-b align-top last:border-b-0">
								<th scope="row" class="px-3 py-1.5 text-left font-normal">
									{key.name || '—'}
									<span class="text-subtle block font-mono text-xs break-all">{key.public_key}</span>
								</th>
								<td class="text-muted px-3 py-1.5">
									<span class="tabular-nums">{timestamp(key.created_at)}</span>
									<span class="block text-xs">by {minter(key.created_by)}</span>
									{#if outlived(key.created_by)}
										<span class="text-warn block text-xs">
											The person who minted it can no longer manage keys here.
										</span>
									{/if}
								</td>
								<td class="text-muted px-3 py-1.5 tabular-nums whitespace-nowrap">
									{key.last_used_at ? timestamp(key.last_used_at) : 'never'}
								</td>
								<td class="px-3 py-1.5">
									<Button
										onclick={() => (
											(notice = null),
											(revoking = revoking === key.public_key ? null : key.public_key)
										)}
										aria-expanded={revoking === key.public_key}
									>
										Revoke
									</Button>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}

		<form
			class="mt-3 flex flex-wrap gap-1.5"
			onsubmit={(event) => (event.preventDefault(), mint())}
		>
			<label for="key-name" class="sr-only">Which program will hold the new key</label>
			<input
				id="key-name"
				bind:value={name}
				maxlength="64"
				autocomplete="off"
				placeholder="Which program will hold it, e.g. checkout api"
				class="border-border bg-canvas placeholder:text-subtle min-w-0 flex-1 rounded-md border
					px-2 py-1 text-sm"
			/>
			<Button type="submit" busy={minting}>
				{#if minting}
					<LoaderCircle class="size-4 animate-spin" />
				{:else}
					<Plus class="size-4" />
				{/if}
				Mint a key pair
			</Button>
		</form>

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
