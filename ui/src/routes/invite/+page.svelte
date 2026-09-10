<script lang="ts">
	import { goto } from '$app/navigation';
	import { passwordProblem, said } from '$lib/accounts';
	import { api } from '$lib/api/client.svelte';
	import { auth } from '$lib/auth.svelte';
	import AuthScreen from '$lib/components/auth/AuthScreen.svelte';
	import Explanation from '$lib/components/auth/Explanation.svelte';
	import Field from '$lib/components/auth/Field.svelte';
	import { begin } from '$lib/session';

	// Accepting an invitation (spec 028 #10), which is also how a lost password
	// is replaced: an owner mints a fresh link and the old one stops working.
	//
	// The email is not asked for and not shown — the token names the account,
	// and a field that had to match would be a way to find out whose link this
	// is. The display name is offered because this is the one moment the person
	// is here and nobody has asked them what to call them.

	const token = auth.tokenFromFragment();
	let name = $state('');
	let password = $state('');
	let again = $state('');
	let busy = $state(false);
	let error = $state<string | null>(null);

	$effect(() => {
		auth.stripFragment();
	});

	async function submit() {
		if (busy || !token) return;
		const problem = passwordProblem(password, again);
		if (problem) {
			error = problem;
			return;
		}
		busy = true;
		error = null;
		try {
			await api.acceptInvite(token, password);
			// Now, and only now, is there a session to hang a name on: the
			// accept endpoint takes a password and nothing else.
			if (name.trim()) await api.patchMe({ name: name.trim() });
			await begin();
			await goto('/traces', { replaceState: true });
		} catch (cause) {
			error = said(cause, 'Something went wrong accepting this invitation.');
		} finally {
			busy = false;
		}
	}
</script>

{#if !token}
	<Explanation title="Accept an invitation">
		This link carries no invitation token. Ask whoever invited you for a fresh link — they are good
		for seven days, and a new one replaces the last.
	</Explanation>
{:else}
	<AuthScreen
		title="Accept an invitation"
		intro="Choose a password. That is all this link is for; it works once."
		submitLabel="Set the password and sign in"
		{busy}
		{error}
		disabled={!password || !again}
		onsubmit={submit}
	>
		<Field label="Display name" bind:value={name} placeholder="optional" autocomplete="name" />
		<Field
			label="Password"
			type="password"
			bind:value={password}
			autocomplete="new-password"
			hint="At least 10 characters. There is no other rule."
		/>
		<Field label="Password again" type="password" bind:value={again} autocomplete="new-password" />
	</AuthScreen>
{/if}
