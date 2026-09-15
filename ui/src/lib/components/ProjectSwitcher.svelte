<script lang="ts">
	import Check from '@lucide/svelte/icons/check';
	import ChevronsUpDown from '@lucide/svelte/icons/chevrons-up-down';
	import Plus from '@lucide/svelte/icons/plus';
	import { DropdownMenu } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type Project } from '$lib/api/client.svelte';
	import { auth } from '$lib/auth.svelte';
	import { project, switchTarget, under } from '$lib/project.svelte';
	import NewProjectDialog from './settings/NewProjectDialog.svelte';

	// The switcher (spec 029 #5), where the sidebar used to print the name:
	// the projects the account can reach, by name, the current one marked,
	// each with its traces of the last day — is it alive — and *New project*
	// for an owner (#7). A menu rather than a select, because a row carries
	// two lines and the last row is an action.
	//
	// The count is asked for on every open and never on load (#8): it is what
	// a person opening the menu wants to know about a project that is not on
	// screen, and nobody wants it about the one that is.

	/** Over this many projects the list gets a box that narrows it. */
	const FILTER_FROM = 8;

	let { open = $bindable(false) }: { open?: boolean } = $props();

	let filter = $state('');
	/** Each reachable project's traces of the last day, once the listing answers. */
	let counts = $state.raw<Map<string, number> | null>(null);
	let creating = $state(false);

	const filtered = $derived(auth.projects.filter(matches));
	const filterable = $derived(auth.projects.length > FILTER_FROM);

	function matches(one: { name: string }) {
		return one.name.toLowerCase().includes(filter.trim().toLowerCase());
	}

	// On every open, however it was opened — a click, or the not-there screen
	// (#4) — and never on load.
	$effect(() => {
		if (!open) return;
		filter = '';
		counts = null;
		let stale = false;
		api
			.listProjects({ activity: '24h' })
			.then(({ projects }) => {
				if (!stale) counts = new Map(projects.map((row) => [row.id, row.traces_24h ?? 0]));
			})
			.catch(() => {
				// The menu is still a menu without the caption: the names and
				// the navigation do not depend on the count.
			});
		return () => {
			stale = true;
		};
	});

	/** Chosen again is nothing happening (edge cases): the menu just closes. */
	function choose(id: string) {
		if (id === project.id) return;
		void goto(switchTarget(page.url, id));
	}

	/** A project just made opens on its dashboard, where the setup is (spec 034 #12). */
	function created(made: Project) {
		void goto(under('/dashboard', made.id));
	}

	function caption(id: string): { text: string; empty: boolean } | null {
		const count = counts?.get(id);
		if (count === undefined) return null;
		if (count === 0) return { text: 'no traces · 24h', empty: true };
		return { text: `${count.toLocaleString('en-US')} ${count === 1 ? 'trace' : 'traces'} · 24h`, empty: false };
	}

	/**
	 * Typing in the box must not be typing in the menu: a menu moves its
	 * focus to the item whose label starts with a character key, and would
	 * take the box's letters as that. Arrows and Escape still reach it.
	 */
	function keep(event: KeyboardEvent) {
		if (event.key.length === 1 || event.key === 'Backspace') event.stopPropagation();
	}

	const row =
		'flex w-full cursor-pointer flex-col items-start rounded-md px-2 py-1.5 text-left ' +
		'text-sm text-fg data-highlighted:bg-raised pointer-coarse:min-h-11';
</script>

<DropdownMenu.Root bind:open>
	<DropdownMenu.Trigger>
		{#snippet child({ props })}
			<button
				{...props}
				aria-label="Switch project"
				class="text-fg hover:bg-raised pointer-coarse:min-h-11 flex min-w-0 max-w-48 flex-1
					items-center gap-1 rounded-md px-2 py-1 text-left text-sm transition-colors
					duration-100 md:w-full md:max-w-none md:flex-none"
			>
				<span class="min-w-0 flex-1 truncate" title={project.name ?? undefined}>
					{#if project.name}
						{project.name}
					{:else}
						<span class="text-subtle">Choose a project</span>
					{/if}
				</span>
				<ChevronsUpDown class="text-subtle size-3.5 shrink-0" />
			</button>
		{/snippet}
	</DropdownMenu.Trigger>
	<DropdownMenu.Portal>
		<DropdownMenu.Content
			align="start"
			sideOffset={4}
			class="border-border bg-canvas shadow-overlay z-50 flex max-h-[min(24rem,70dvh)] w-64
				flex-col rounded-md border p-1"
		>
			{#if filterable}
				<!-- Eight is where a column of names stops fitting in one glance. -->
				<input
					type="search"
					bind:value={filter}
					onkeydown={keep}
					placeholder="Filter projects"
					aria-label="Filter projects"
					autocomplete="off"
					spellcheck="false"
					class="border-border bg-canvas text-fg placeholder:text-subtle mb-1 w-full shrink-0
						rounded-md border px-2 py-1 text-sm"
				/>
			{/if}
			<DropdownMenu.RadioGroup
				value={project.id ?? ''}
				onValueChange={choose}
				class="min-h-0 overflow-y-auto"
				aria-label="Projects"
			>
				{#each filtered as one (one.id)}
					{@const traffic = caption(one.id)}
					<DropdownMenu.RadioItem value={one.id} class={row} closeOnSelect>
						{#snippet children({ checked })}
							<span class="flex w-full items-center gap-2">
								<span class="min-w-0 flex-1 truncate">{one.name}</span>
								{#if checked}
									<Check class="text-accent size-4 shrink-0" aria-label="Current" />
								{/if}
							</span>
							{#if traffic}
								<span class={['text-xs', traffic.empty ? 'text-subtle' : 'text-muted']}>
									{traffic.text}
								</span>
							{/if}
						{/snippet}
					</DropdownMenu.RadioItem>
				{:else}
					<p class="text-subtle px-2 py-1.5 text-sm">
						{auth.projects.length === 0 ? 'No projects yet' : 'No project matches'}
					</p>
				{/each}
			</DropdownMenu.RadioGroup>
			{#if auth.owner}
				<DropdownMenu.Separator class="bg-border my-1 h-px" />
				<DropdownMenu.Item onSelect={() => (creating = true)}>
					{#snippet child({ props })}
						<button {...props} type="button" class="{row} text-muted flex-row items-center gap-2">
							<Plus class="size-4 shrink-0" />
							New project
						</button>
					{/snippet}
				</DropdownMenu.Item>
			{/if}
		</DropdownMenu.Content>
	</DropdownMenu.Portal>
</DropdownMenu.Root>

{#if auth.owner}
	<NewProjectDialog open={creating} onclose={() => (creating = false)} oncreated={created} />
{/if}
