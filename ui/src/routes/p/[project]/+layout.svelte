<script lang="ts">
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { auth } from '$lib/auth.svelte';
	import { switcher } from '$lib/project.svelte';

	// The screen behind an id the account cannot reach (spec 029 #4): unknown,
	// malformed, or a membership taken away under an open tab. One sentence,
	// the switcher open beside it, and the screen the URL names never mounts,
	// so nothing asks the server with an id it would refuse. The interface
	// does not say which of the two it is because the server does not either.
	//
	// Decided at the navigation, not at every `me` (#4, edge cases): a project
	// deleted under an open tab is the not-there screen on the *next*
	// navigation. Deciding it live would pull the Server tab out from under
	// the owner who just deleted the project from it — with the Restore
	// button, and for their only project the one place Restore is. `page.params`
	// is replaced on every navigation (#12), so that is what this follows.

	let { children } = $props();

	const reachable = $derived.by(() => {
		const id = page.params.project;
		return untrack(() => auth.projects.some((one) => one.id === id));
	});

	// Per navigation, not per transition: closing the menu and then following
	// a sidebar link is another not-there screen, and the menu opens again.
	$effect(() => {
		void page.params.project;
		if (!reachable) switcher.open = true;
	});
</script>

{#if reachable}
	<!-- Keyed on the id: a switch keeps the section (spec 029 #6), so the
	     screen that renders `/p/b/traces` is the one that rendered
	     `/p/a/traces`, and every read it holds was about the other project.
	     A remount is what "another project" means — nothing carries over. -->
	{#key page.params.project}
		{@render children()}
	{/key}
{:else}
	<div class="flex flex-1 items-center justify-center p-8">
		<div class="max-w-md text-center">
			<p class="text-subtle font-mono text-xs break-all">{page.params.project}</p>
			<h1 class="mt-1 text-xl font-semibold">This project is not yours to see</h1>
			<p class="text-muted mt-2">
				It does not exist, or this account is not a member of it. Pick one of yours from the
				switcher.
			</p>
		</div>
	</div>
{/if}
