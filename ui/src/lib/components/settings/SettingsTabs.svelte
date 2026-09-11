<script lang="ts">
	import { Tabs } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { auth } from '$lib/auth.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import { project, under, within } from '$lib/project.svelte';

	// Settings was one screen while one credential unlocked all of it. Three
	// tabs are three audiences (spec 028 #14): this project, this account, and
	// this server — the last of which only an owner has, so the tab is absent
	// rather than disabled for everybody else, and the route redirects.
	//
	// The active tab is the URL (Application contract), so a tab is a link
	// somebody can send. Two of the three are about a project and live under
	// its prefix: `/p/{id}/settings/server` opens on the accounts table. The
	// Account tab is about the person and lives bare, at `/settings/account`
	// (spec 029 #14): an account that reaches no project still has a name and
	// a password. On that bare screen the other two tabs point at the
	// remembered project, and are absent when there is none — as the
	// sidebar's sections are on `/p`.

	let { children } = $props();

	/** The project the Project and Server tabs are about. */
	const about = $derived(page.params.project ?? project.remembered());
	const active = $derived(within(page.url.pathname).split('/')[2] || 'project');

	const tabs = $derived([
		...(about ? [{ value: 'project', label: 'Project', href: under('/settings/project', about) }] : []),
		{ value: 'account', label: 'Account', href: '/settings/account' },
		...(about && auth.owner
			? [{ value: 'server', label: 'Server', href: under('/settings/server', about) }]
			: [])
	]);

	function open(value: string) {
		const tab = tabs.find((one) => one.value === value);
		if (tab && tab.value !== active) void goto(tab.href);
	}
</script>

<PageHeader title="Settings" />

<Tabs.Root value={active} onValueChange={open} class="flex min-h-0 flex-1 flex-col">
	<Tabs.List class="border-border flex shrink-0 gap-1 border-b px-4">
		{#each tabs as tab (tab.value)}
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
