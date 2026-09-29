<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Plus from '@lucide/svelte/icons/plus';
	import { said } from '$lib/accounts';
	import { api, type DryRun, type Key, type NewKey, type Project } from '$lib/api/client.svelte';
	import { Fold } from '$lib/fold.svelte';
	import { timeOrNever, timestamp } from '$lib/format';
	import { MAX_KEY_NAME, minter, outlived, SCOPES, type Scope, tooLong } from '$lib/keys';
	import Button from '../Button.svelte';
	import ConfirmCard from '../ConfirmCard.svelte';
	import Folded from '../Folded.svelte';
	import SecretDialog from '../SecretDialog.svelte';
	import Card from './Card.svelte';
	import ViewerNote from './ViewerNote.svelte';

	// API keys (spec 007 #9). Several active pairs are what makes rotation
	// zero-downtime: mint, move the SDKs, revoke.
	//
	// The secret appears once, in the dialog, and the list shows public keys
	// only — because that is all the server has. Revoking asks twice: once here
	// and, when it is the project's last key or its last `ingest` key, again
	// with the echo the server demands (spec 005 #12, spec 045 #11).
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
	/** What it may do: `ingest` unless changed — the least a program needs (spec 045 #14). */
	let scopes = $state<Scope[]>(['ingest']);
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
			// In the server's order, whatever order they were ticked in.
			const chosen = SCOPES.filter((s) => scopes.includes(s.scope)).map((s) => s.scope);
			minted = await api.createKey(current.id, chosen, name.trim());
			name = '';
			scopes = ['ingest'];
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

	// In a box narrower than the table the row is the key and *Revoke*; its
	// scopes, who minted it when and its last use fold under the public key
	// (spec 006 #24), in a plain cell for the reason the Accounts card gives
	// (#18), so the button carries the public key. The number is the unfolded
	// table's width and its `min-width`.
	const fold = new Fold(640);
	const narrow = $derived(fold.narrow);
</script>

<Card
	title="API keys"
	description="Each pair may do what its scopes say, and a key's scopes never change. Rotate by
		minting a new one, moving your programs over, then revoking the old: the old one's last use
		says when nothing holds it any more."
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
			<div bind:contentRect={fold.rect} class="border-border overflow-x-auto rounded-md border">
				<table class="w-full border-collapse text-left text-sm" style:min-width={fold.min}>
					<thead class="text-subtle text-xs whitespace-nowrap">
						<tr class="border-border border-b">
							<th scope="col" class={['px-3 py-1.5 font-medium', narrow && 'w-full']}>Name</th>
							{#if !narrow}
								<th scope="col" class="px-3 py-1.5 font-medium">Scopes</th>
								<th scope="col" class="px-3 py-1.5 font-medium">Created</th>
								<th scope="col" class="px-3 py-1.5 font-medium">Last used</th>
							{/if}
							<th scope="col" class="w-24 px-3 py-1.5"><span class="sr-only">Actions</span></th>
						</tr>
					</thead>
					<tbody>
						{#each keys as key (key.public_key)}
							{@const warning = outlived(key.created_by)}
							<tr class="border-border border-b align-top last:border-b-0">
								{#if narrow}
									<td class="max-w-0 px-3 py-1.5 wrap-anywhere">
										{@render identity(key, true)}
										<div class="text-muted text-xs">
											<Folded
												values={[
													key.scopes.join(', '),
													`created ${timestamp(key.created_at)}`,
													`by ${minter(key.created_by)}`,
													`last used ${timeOrNever(key.last_used_at)}`
												]}
											/>
										</div>
										{#if warning}{@render outlivedNote()}{/if}
									</td>
								{:else}
									<th scope="row" class="min-w-40 px-3 py-1.5 text-left font-normal wrap-anywhere">
										{@render identity(key)}
									</th>
									<td class="text-muted px-3 py-1.5">{key.scopes.join(', ')}</td>
									<td class="text-muted max-w-0 min-w-44 px-3 py-1.5">
										<span class="tabular-nums whitespace-nowrap">{timestamp(key.created_at)}</span>
										<span class="block truncate text-xs" title={minter(key.created_by)}>
											by {minter(key.created_by)}
										</span>
										{#if warning}{@render outlivedNote()}{/if}
									</td>
									<td class="text-muted px-3 py-1.5 tabular-nums whitespace-nowrap">
										{timeOrNever(key.last_used_at)}
									</td>
								{/if}
								<td class="px-3 py-1.5">
									<Button
										aria-label={narrow ? `Revoke ${key.public_key}` : undefined}
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

		<form class="mt-3" onsubmit={(event) => (event.preventDefault(), mint())}>
			<label for="key-name" class="sr-only">Which program will hold the new key</label>
			<input
				id="key-name"
				bind:value={name}
				autocomplete="off"
				aria-invalid={tooLong(name) || undefined}
				placeholder="Which program will hold it, e.g. checkout api"
				class="border-border bg-canvas placeholder:text-subtle w-full rounded-md border px-2 py-1
					text-sm"
			/>
			{#if tooLong(name)}
				<p role="alert" class="text-danger mt-1 text-sm">
					A key's name is at most {MAX_KEY_NAME} characters.
				</p>
			{/if}
			<fieldset class="mt-2">
				<legend class="text-muted mb-1 text-xs font-medium">What it may do</legend>
				{#each SCOPES as { scope, does } (scope)}
					<label class="flex items-start gap-2 py-0.5 text-sm">
						<input type="checkbox" value={scope} bind:group={scopes} class="mt-0.5 size-4 shrink-0" />
						<span><span class="font-mono">{scope}</span> <span class="text-muted">— {does}</span></span>
					</label>
				{/each}
			</fieldset>
			<Button type="submit" class="mt-2" busy={minting} disabled={tooLong(name) || !scopes.length}>
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
					description="Anything still using this pair stops being able to. If it is the project's
						last key, or the last one that can ingest, ingest stops until another is minted — and
						the server will ask for the project's name first."
					echoLabel="project name"
					previewLabel="Revoke"
					executeLabel="Revoke it anyway"
					subject={publicKey}
					preview={() => revoke(publicKey)}
					execute={(confirm) => revoke(publicKey, confirm) as Promise<string>}
					ondone={() => (revoking = null)}
				/>
			</div>
		{/if}
	{/if}
</Card>

<!-- The public key is what a person copies into a program, so unfolded it is
     never torn across lines, and the column is as wide as it needs (spec 006
     #24). Folded, the cell is the box's width, and it breaks last. -->
{#snippet identity(key: Key, stacked = false)}
	{key.name || '—'}
	<span class={['text-subtle block font-mono text-xs', stacked ? 'break-all' : 'whitespace-nowrap']}>
		{key.public_key}
	</span>
{/snippet}

{#snippet outlivedNote()}
	<span class="text-warn block text-xs">
		The person who minted it can no longer manage keys here.
	</span>
{/snippet}

<SecretDialog pair={minted} onclose={() => (minted = null)} />
