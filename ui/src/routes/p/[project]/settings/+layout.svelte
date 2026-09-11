<script lang="ts">
	import { Tabs } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { auth } from '$lib/auth.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import { href, within } from '$lib/project.svelte';

	// Settings was one screen while one credential unlocked all of it. Three
	// tabs are three audiences (spec 028 #14): this project, this account, and
	// this server — the last of which only an owner has, so the tab is absent
	// rather than disabled for everybody else, and the route redirects.
	//
	// The active tab is the URL (Application contract), so a tab is a link
	// somebody can send: `/p/{id}/settings/server` opens on the accounts
	// table. The Server tab is about the whole server and lives under the
	// prefix anyway (spec 029 #1): one rule for what is inside the shell.

	let { children } = $props();

	const TABS = [
		{ value: 'project', label: 'Project' },
		{ value: 'account', label: 'Account' },
		{ value: 'server', label: 'Server', owners: true }
	];

	const visible = $derived(TABS.filter((tab) => !tab.owners || auth.owner));
	const active = $derived(within(page.url.pathname).split('/')[2] || 'project');
</script>

<PageHeader title="Settings" />

<Tabs.Root
	value={active}
	onValueChange={(value) => goto(href(`/settings/${value}`))}
	class="flex min-h-0 flex-1 flex-col"
>
	<Tabs.List class="border-border flex shrink-0 gap-1 border-b px-4">
		{#each visible as tab (tab.value)}
			<Tabs.Trigger
				value={tab.value}
				class="text-muted hover:text-fg data-[state=active]:border-accent data-[state=active]:text-fg
					pointer-coarse:min-h-11 cursor-pointer border-b-2 border-transparent px-3 py-2 text-sm
					data-[state=active]:font-medium"
			>
				{tab.label}
			</Tabs.Trigger>
		{/each}
	</Tabs.List>

	<!-- One panel, whose value is whichever tab is active: the content is the
	     child route, so the router decides what is in it and bits-ui keeps the
	     keyboard and the aria wiring (spec 006 #3). -->
	<Tabs.Content value={active} class="min-h-0 flex-1 overflow-auto p-4">
		<div class="mx-auto flex max-w-3xl flex-col gap-4">{@render children()}</div>
	</Tabs.Content>
</Tabs.Root>
