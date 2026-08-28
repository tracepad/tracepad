<script lang="ts">
	import '../app.css';
	import { page } from '$app/state';
	import { auth, LOGIN_ROUTE } from '$lib/auth.svelte';
	import { project } from '$lib/project.svelte';
	import Sidebar from '$lib/components/Sidebar.svelte';

	let { children } = $props();

	// The login form is the one screen outside the shell: there is nothing to
	// navigate to yet, and a sidebar that names a project nobody is signed
	// into would be an odd thing to look at.
	const inShell = $derived(page.url.pathname !== LOGIN_ROUTE);

	$effect(() => {
		// The router exists by now, so the pre-authed key can leave the
		// address bar (spec 006 #8).
		auth.stripFragment();
	});

	$effect(() => {
		if (auth.authenticated) project.load();
	});
</script>

{#if inShell}
	<!-- `dvh` rather than `vh`: on a phone the browser chrome is part of the
	     viewport and `100vh` puts the last row under it (spec 006 #15). -->
	<div class="flex h-dvh flex-col md:flex-row">
		<Sidebar />
		<main class="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
			{@render children()}
		</main>
	</div>
{:else}
	{@render children()}
{/if}
