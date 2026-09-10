<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { said } from '$lib/accounts';
	import { api } from '$lib/api/client.svelte';
	import { returnTo } from '$lib/auth.svelte';
	import AuthScreen from '$lib/components/auth/AuthScreen.svelte';
	import Field from '$lib/components/auth/Field.svelte';
	import { begin } from '$lib/session';

	// An email and a password (spec 028 #13). No key is accepted here any more:
	// a project key is what a program holds, and a person holds an account.
	//
	// Every failure the server can have — wrong email, wrong password, disabled,
	// invited and never accepted — comes back as one sentence, and this screen
	// prints it rather than guessing which of them it was (Decision 8).

	let email = $state('');
	let password = $state('');
	let busy = $state(false);
	let error = $state<string | null>(null);

	// Coming back to where the guard interrupted, which is what makes a deep
	// link survive a sign-in.
	const next = $derived(returnTo(page.url));

	async function submit() {
		if (busy) return;
		busy = true;
		error = null;
		try {
			await api.login(email.trim(), password);
			await begin();
			await goto(next, { replaceState: true });
		} catch (cause) {
			error = said(cause, 'Something went wrong signing in.');
		} finally {
			busy = false;
		}
	}
</script>

<AuthScreen
	title="Sign in"
	intro="Sign in to read this server's traces."
	submitLabel="Sign in"
	{busy}
	{error}
	disabled={!email.trim() || !password}
	onsubmit={submit}
>
	<Field label="Email" type="email" bind:value={email} autocomplete="username" />
	<Field label="Password" type="password" bind:value={password} autocomplete="current-password" />

	{#snippet footer()}
		Lost your password? An owner can send you a new invitation link, which sets a new one.
	{/snippet}
</AuthScreen>
