<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { untrack } from 'svelte';
	import { passwordProblem, said } from '$lib/accounts';
	import { api } from '$lib/api/client.svelte';
	import { auth } from '$lib/auth.svelte';
	import Button from '../Button.svelte';
	import Card from './Card.svelte';
	import { refresh } from '$lib/session';

	// What everybody can change about themselves (spec 028 #14): what to call
	// them, and the password they sign in with. The email is shown and not
	// editable — it is the sign-in name and an owner's to change (Decision 1).

	// Seeded once and then owned by whoever is typing: the `me` that follows a
	// save must not reach into the field under them.
	let name = $state(untrack(() => auth.account?.name ?? ''));
	let savingName = $state(false);
	let nameFailure = $state<string | null>(null);
	let nameDone = $state(false);

	let currentPassword = $state('');
	let nextPassword = $state('');
	let again = $state('');
	let savingPassword = $state(false);
	let passwordFailure = $state<string | null>(null);
	let passwordDone = $state(false);

	const renamed = $derived(name.trim() !== (auth.account?.name ?? ''));

	async function saveName() {
		savingName = true;
		nameFailure = null;
		nameDone = false;
		try {
			await api.patchMe({ name: name.trim() });
			await refresh();
			nameDone = true;
		} catch (cause) {
			nameFailure = said(cause, 'The change failed.');
		} finally {
			savingName = false;
		}
	}

	async function changePassword() {
		const problem = passwordProblem(nextPassword, again);
		if (problem) {
			passwordFailure = problem;
			passwordDone = false;
			return;
		}
		savingPassword = true;
		passwordFailure = null;
		passwordDone = false;
		try {
			await api.patchMe({ password: { current: currentPassword, new: nextPassword } });
			currentPassword = '';
			nextPassword = '';
			again = '';
			passwordDone = true;
		} catch (cause) {
			passwordFailure = said(cause, 'The change failed.');
		} finally {
			savingPassword = false;
		}
	}

	const field = 'border-border bg-canvas w-full rounded-md border px-2 py-1 text-sm';
</script>

<Card title="Account" description="How you sign in, and what this server calls you.">
	<dl class="text-muted mb-4 flex items-baseline gap-1.5 text-sm">
		<dt class="text-subtle text-xs">Email</dt>
		<dd class="truncate">{auth.account?.email ?? ''}</dd>
	</dl>

	<div class="flex flex-wrap items-end gap-2">
		<div class="min-w-0 flex-1">
			<label for="account-name" class="text-muted mb-1 block text-xs font-medium">
				Display name
			</label>
			<input
				id="account-name"
				type="text"
				bind:value={name}
				autocomplete="name"
				maxlength={200}
				class={field}
			/>
		</div>
		<Button variant="primary" onclick={saveName} disabled={!renamed} busy={savingName}>
			{#if savingName}<LoaderCircle class="size-4 animate-spin" />{/if}
			Save
		</Button>
	</div>
	{#if nameFailure}
		<p role="alert" class="text-danger mt-2 text-sm">{nameFailure}</p>
	{:else if nameDone}
		<p role="status" class="text-ok mt-2 text-sm">Saved.</p>
	{/if}
</Card>

<Card
	title="Password"
	description="Changing it signs out every other browser you are signed in on, because that is what
		a person does when they think somebody else has it."
>
	<div class="grid gap-3 sm:max-w-sm">
		<div>
			<label for="current-password" class="text-muted mb-1 block text-xs font-medium">
				Current password
			</label>
			<input
				id="current-password"
				type="password"
				bind:value={currentPassword}
				autocomplete="current-password"
				class={field}
			/>
		</div>
		<div>
			<label for="new-password" class="text-muted mb-1 block text-xs font-medium">
				New password
			</label>
			<input
				id="new-password"
				type="password"
				bind:value={nextPassword}
				autocomplete="new-password"
				class={field}
			/>
		</div>
		<div>
			<label for="new-password-again" class="text-muted mb-1 block text-xs font-medium">
				New password again
			</label>
			<input
				id="new-password-again"
				type="password"
				bind:value={again}
				autocomplete="new-password"
				class={field}
			/>
		</div>
	</div>

	<Button
		class="mt-3"
		variant="primary"
		onclick={changePassword}
		disabled={!currentPassword || !nextPassword || !again}
		busy={savingPassword}
	>
		{#if savingPassword}<LoaderCircle class="size-4 animate-spin" />{/if}
		Change the password
	</Button>

	{#if passwordFailure}
		<p role="alert" class="text-danger mt-2 text-sm">{passwordFailure}</p>
	{:else if passwordDone}
		<p role="status" class="text-ok mt-2 text-sm">
			Changed. Every other browser was signed out.
		</p>
	{/if}
</Card>
