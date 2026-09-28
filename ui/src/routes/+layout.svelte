<script lang="ts">
	import '../app.css';
	import { MediaQuery } from 'svelte/reactivity';
	import { page } from '$app/state';
	import { OUTSIDE_THE_SHELL } from '$lib/auth.svelte';
	import PhoneBar from '$lib/components/PhoneBar.svelte';
	import PhoneTabs from '$lib/components/PhoneTabs.svelte';
	import Sidebar from '$lib/components/Sidebar.svelte';
	import { PHONE } from '$lib/phone';

	let { children } = $props();

	// Three screens sit outside the shell (spec 028 #13): signing in, setting
	// up the first owner, and accepting an invitation. A sidebar that named a
	// project nobody is signed into would be an odd thing to look at, and on
	// two of the three there is not even a session to name one from.
	const inShell = $derived(!OUTSIDE_THE_SHELL.includes(page.url.pathname));

	// A column on a desktop; on a phone a bar on top and tabs under the page
	// (spec 006 #20), in the document in the order they are on the screen.
	const phone = new MediaQuery(PHONE);
</script>

{#if inShell}
	<!-- `dvh` rather than `vh`: on a phone the browser chrome is part of the
	     viewport and `100vh` puts the last row under it (spec 006 #15). -->
	<div class="flex h-dvh flex-col md:flex-row">
		{#if phone.current}<PhoneBar />{:else}<Sidebar />{/if}
		<main class="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
			{@render children()}
		</main>
		{#if phone.current}<PhoneTabs />{/if}
	</div>
{:else}
	{@render children()}
{/if}
