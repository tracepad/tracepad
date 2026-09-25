<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import X from '@lucide/svelte/icons/x';
	import { Dialog } from 'bits-ui';
	import { said } from '$lib/accounts';
	import {
		api,
		type AccountDetail,
		type Invitation,
		type MemberRole,
		type Project
	} from '$lib/api/client.svelte';
	import Button from '../Button.svelte';

	// Inviting somebody, and editing them afterwards (spec 028 #14). One dialog
	// for both, because they ask for the same things: a name, whether they run
	// the server, and which projects they can reach with what role.
	//
	// An owner has every project, so ticking the box empties the membership
	// rows — the server does that (Decision 12) and the form says so rather
	// than offering a list that would be thrown away.

	let {
		/** Null while inviting somebody new; the account being edited otherwise. */
		account,
		/** The live projects a membership can name. */
		projects,
		oninvited,
		onsaved,
		onclose
	}: {
		account: AccountDetail | null;
		projects: Project[];
		oninvited: (invitation: Invitation) => void;
		onsaved: () => Promise<void>;
		onclose: () => void;
	} = $props();

	const editing = $derived(account !== null);

	let email = $state('');
	let name = $state('');
	let owner = $state(false);
	let disabled = $state(false);
	/** The role wanted in each project; a project absent from the map has none. */
	let roles = $state<Record<string, MemberRole | ''>>({});
	let busy = $state(false);
	let failure = $state<string | null>(null);

	// Seeded whenever the dialog opens on a different account, and never while
	// it is open: a re-read behind the dialog must not rewrite what is being
	// typed into it.
	$effect(() => {
		const opened = account;
		email = opened?.email ?? '';
		name = opened?.name ?? '';
		owner = opened?.owner ?? false;
		disabled = opened?.disabled ?? false;
		const wanted: Record<string, MemberRole | ''> = {};
		for (const one of opened?.projects ?? []) {
			if (one.role !== 'owner') wanted[one.id] = one.role;
		}
		roles = wanted;
		failure = null;
	});

	/** The memberships as the API spells them; empty for an owner. */
	function wanted() {
		if (owner) return [];
		return Object.entries(roles)
			.filter(([, role]) => role !== '')
			.map(([project_id, role]) => ({ project_id, role: role as MemberRole }));
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (busy) return;
		busy = true;
		failure = null;
		try {
			if (account) await save(account);
			else oninvited(await api.createAccount({ email: email.trim(), name: name.trim(), owner, memberships: wanted() }));
			await onsaved();
			onclose();
		} catch (cause) {
			// The server's own words: "the last owner", "an owner has every
			// project", "email taken" (spec 007 #4).
			failure = said(cause, 'The change failed.');
		} finally {
			busy = false;
		}
	}

	/**
	 * The edit, in the order that cannot lose a row: the flags first, because
	 * making somebody an owner drops their memberships, and the memberships
	 * after, because they only mean anything for somebody who is not one.
	 */
	async function save(target: AccountDetail) {
		await api.patchAccount(target.id, { name: name.trim(), owner, disabled });
		if (owner) return;
		const had = new Map(target.projects.map((one) => [one.id, one.role]));
		for (const project of projects) {
			// A project this account is not in reads as `''`, the same as "no
			// access" does, so that the two compare equal: without it every
			// save sent a `DELETE` for every project on the server.
			const before = had.get(project.id) ?? '';
			const after = roles[project.id] || '';
			if (before === after) continue;
			if (after === '') await api.deleteMembership(target.id, project.id);
			else await api.putMembership(target.id, project.id, after);
		}
	}

	async function reinvite(target: AccountDetail) {
		busy = true;
		failure = null;
		try {
			const fresh = await api.inviteAccount(target.id);
			oninvited({ account: target, ...fresh });
			await onsaved();
			onclose();
		} catch (cause) {
			failure = said(cause, 'Failed to mint an invitation link.');
		} finally {
			busy = false;
		}
	}

	const field = 'border-border bg-canvas w-full rounded-md border px-2 py-1 text-sm';
</script>

<Dialog.Root open onOpenChange={(open) => !open && !busy && onclose()}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="border-border bg-canvas shadow-overlay fixed top-1/2 left-1/2 z-50 max-h-[90dvh]
				w-[min(32rem,calc(100vw-1.5rem))] -translate-x-1/2 -translate-y-1/2 overflow-y-auto
				rounded-lg border p-4"
		>
			<form onsubmit={submit}>
				<div class="flex items-start gap-3">
					<div class="min-w-0 flex-1">
						<Dialog.Title class="text-lg font-semibold">
							{editing ? 'Edit the account' : 'Invite somebody'}
						</Dialog.Title>
						<Dialog.Description class="text-muted mt-1 text-sm">
							{editing
								? 'Changes take effect at once. Disabling ends their sessions.'
								: 'They get a link that sets their password. No mail is sent — you carry it to them.'}
						</Dialog.Description>
					</div>
					<Dialog.Close>
						{#snippet child({ props })}
							<button
								{...props}
								type="button"
								aria-label="Close"
								class="text-muted hover:bg-raised hover:text-fg pointer-coarse:size-11 inline-flex
									size-7 shrink-0 cursor-pointer items-center justify-center rounded-md
									transition-colors duration-100"
							>
								<X class="size-4" />
							</button>
						{/snippet}
					</Dialog.Close>
				</div>

				<div class="mt-4 grid gap-3">
					<div>
						<label for="account-email" class="text-muted mb-1 block text-xs font-medium">
							Email
						</label>
						<input
							id="account-email"
							type="email"
							bind:value={email}
							disabled={editing}
							autocomplete="off"
							spellcheck="false"
							placeholder="helper@example.com"
							class="{field} disabled:opacity-60"
						/>
						{#if editing}
							<p class="text-subtle mt-1 text-xs">
								The email is the sign-in name and the echo a deletion is typed against, so it does
								not change here.
							</p>
						{/if}
					</div>

					<div>
						<label for="account-display" class="text-muted mb-1 block text-xs font-medium">
							Display name
						</label>
						<input
							id="account-display"
							type="text"
							bind:value={name}
							placeholder="optional"
							autocomplete="off"
							maxlength={200}
							class={field}
						/>
					</div>

					<label class="flex items-center gap-2 text-sm">
						<input type="checkbox" bind:checked={owner} class="size-4" />
						Owner — every project, and the accounts themselves
					</label>

					{#if editing}
						<label class="flex items-center gap-2 text-sm">
							<input type="checkbox" bind:checked={disabled} class="size-4" />
							Disabled — signed out at once, and cannot sign in again
						</label>
					{/if}
				</div>

				<fieldset class="mt-4" disabled={owner}>
					<legend class="text-muted mb-1 text-xs font-medium">Projects</legend>
					{#if owner}
						<p class="text-subtle text-sm">
							An owner reaches every project, present and future, so there is nothing to pick.
						</p>
					{:else if projects.length === 0}
						<p class="text-subtle text-sm">This server has no projects yet.</p>
					{:else}
						<ul class="border-border divide-border divide-y rounded-md border">
							{#each projects as row (row.id)}
								<li class="flex items-center gap-2 px-3 py-1.5">
									<span class="min-w-0 flex-1 truncate text-sm">{row.name}</span>
									<label class="sr-only" for="role-{row.id}">Role in {row.name}</label>
									<select
										id="role-{row.id}"
										value={roles[row.id] ?? ''}
										onchange={(event) =>
											(roles = { ...roles, [row.id]: event.currentTarget.value as MemberRole | '' })}
										class="border-border bg-canvas rounded-md border px-2 py-1 text-sm"
									>
										<option value="">No access</option>
										<option value="viewer">Viewer</option>
										<option value="editor">Editor</option>
									</select>
								</li>
							{/each}
						</ul>
					{/if}
				</fieldset>

				{#if failure}
					<p role="alert" class="text-danger mt-3 text-sm">{failure}</p>
				{/if}

				<div class="mt-4 flex flex-wrap justify-end gap-1.5">
					{#if account}
						{@const target = account}
						<Button class="mr-auto" onclick={() => reinvite(target)} disabled={busy}>
							New invitation link
						</Button>
					{/if}
					<Dialog.Close>
						{#snippet child({ props })}
							<Button {...props} disabled={busy}>Cancel</Button>
						{/snippet}
					</Dialog.Close>
					<Button
						type="submit"
						variant="primary"
						{busy}
						disabled={!editing && !email.trim()}
					>
						{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
						{editing ? 'Save' : 'Invite'}
					</Button>
				</div>
			</form>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
