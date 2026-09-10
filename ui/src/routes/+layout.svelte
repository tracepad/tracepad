<script lang="ts">
	import '../app.css';
	import { page } from '$app/state';
	import { OUTSIDE_THE_SHELL } from '$lib/auth.svelte';
	import Sidebar from '$lib/components/Sidebar.svelte';

	let { children } = $props();

	// Three screens sit outside the shell (spec 028 #13): signing in, setting
	// up the first owner, and accepting an invitation. A sidebar that named a
	// project nobody is signed into would be an odd thing to look at, and on
	// two of the three there is not even a session to name one from.
	const inShell = $derived(!OUTSIDE_THE_SHELL.includes(page.url.pathname));
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
