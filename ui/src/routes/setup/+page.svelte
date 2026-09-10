<script lang="ts">
	import { goto } from '$app/navigation';
	import { passwordProblem, said } from '$lib/accounts';
	import { api } from '$lib/api/client.svelte';
	import { auth } from '$lib/auth.svelte';
	import AuthScreen from '$lib/components/auth/AuthScreen.svelte';
	import Explanation from '$lib/components/auth/Explanation.svelte';
	import Field from '$lib/components/auth/Field.svelte';
	import { begin } from '$lib/session';

	// The first owner (spec 028 #9). The server prints this link at every start
	// while nobody can sign in, and the token rides in the fragment — which a
	// browser never puts on the wire, so a link pasted into a terminal or a chat
	// does not leave the secret in a log on the way.

	// Read before the router touches the address bar, and taken back out of it
	// as soon as the router exists (spec 006 #8).
	const token = auth.tokenFromFragment();
	let name = $state('');
	let email = $state('');
	let password = $state('');
	let again = $state('');
	let busy = $state(false);
	let error = $state<string | null>(null);
	/** Null while the server has not said whether it still needs an owner. */
	let required = $state.raw<boolean | null>(null);

	$effect(() => {
		auth.stripFragment();
	});

	$effect(() => {
		api
			.getSetup()
			.then((answer) => (required = answer.required))
			.catch(() => (required = null));
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
			await api.setup({ token, email: email.trim(), password, name: name.trim() });
			await begin();
			await goto('/traces', { replaceState: true });
		} catch (cause) {
			error = said(cause, 'Something went wrong setting this server up.');
		} finally {
			busy = false;
		}
	}
</script>

{#if required === false || !token}
	<Explanation title="Set up">
		{#if token}
			This server already has an owner, so there is nothing to set up.
		{:else}
			This link carries no setup token. The server prints a complete one at every start until it
			has an owner.
		{/if}
	</Explanation>
{:else}
	<AuthScreen
		title="Set up"
		intro="Create the first owner of this server. An owner has every project and manages the accounts."
		submitLabel="Create the owner"
		{busy}
		{error}
		disabled={!email.trim() || !password || !again}
		onsubmit={submit}
	>
		<Field label="Display name" bind:value={name} placeholder="optional" autocomplete="name" />
		<Field label="Email" type="email" bind:value={email} autocomplete="username" />
		<Field
			label="Password"
			type="password"
			bind:value={password}
			autocomplete="new-password"
			hint="At least 10 characters. There is no other rule."
		/>
		<Field
			label="Password again"
			type="password"
			bind:value={again}
			autocomplete="new-password"
		/>
	</AuthScreen>
{/if}
