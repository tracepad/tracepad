<script lang="ts">
	import { MediaQuery } from 'svelte/reactivity';
	import { api } from '$lib/api/client.svelte';
	import { PHONE } from '$lib/phone';
	import { href, switcher } from '$lib/project.svelte';
	import { active, isGroup, SECTIONS, type Item } from '$lib/sections';
	import AccountMenu from './AccountMenu.svelte';
	import PhoneTabs from './PhoneTabs.svelte';
	import ProjectSwitcher from './ProjectSwitcher.svelte';
	import ThemeToggle from './ThemeToggle.svelte';

	// Two shapes for one list of destinations: a column down the left on a
	// desktop, and on a phone a slim bar on top with tabs under the thumb
	// (spec 006 #20), which is markup of its own. One of them is in the
	// document at a time, so no link is there twice.
	const phone = new MediaQuery(PHONE);

	const link =
		'flex items-center gap-2 rounded-md px-2 py-1.5 transition-colors duration-100 pointer-coarse:min-h-11';
	const lit = 'bg-accent-soft text-accent font-medium';
	const dim = 'text-muted hover:bg-raised hover:text-fg';
</script>

{#snippet item(entry: Item, indented: boolean)}
	{@const Glyph = entry.icon}
	<li>
		<a
			href={href(entry.href)}
			aria-current={active(entry.href) ? 'page' : undefined}
			class={[link, indented && 'ml-3', active(entry.href) ? lit : dim]}
		>
			<Glyph class="size-4 shrink-0" />
			{entry.label}
		</a>
	</li>
{/snippet}

{#if phone.current}
	<PhoneTabs />
{:else}
	<aside class="border-border bg-surface flex h-full w-52 shrink-0 flex-col border-r">
		<div class="flex h-12 shrink-0 items-center px-3">
			<span class="text-lg font-semibold tracking-tight">Tracepad</span>
		</div>

		<!-- The switcher (spec 029 #5), in the space the project name had. -->
		<div class="flex min-w-0 items-center px-2 pb-2">
			<ProjectSwitcher bind:open={switcher.open} />
		</div>

		<nav class="min-w-0 flex-1 px-2" aria-label="Sections">
			<!-- A group's label is a heading over its children, not a link (#1). -->
			<ul>
				{#each SECTIONS as section (section.label)}
					{#if isGroup(section)}
						<li>
							<span
								class="text-subtle mt-2 block px-2 py-1 text-xs font-medium tracking-wide uppercase"
							>
								{section.label}
							</span>
							<ul>
								{#each section.children as child (child.href)}
									{@render item(child, true)}
								{/each}
							</ul>
						</li>
					{:else}
						{@render item(section, false)}
					{/if}
				{/each}
			</ul>
		</nav>

		<!-- Who is signed in sits at the bottom of the column (spec 028 #14):
		     the last thing on the way out, and the one control that is about
		     the reader rather than the data. -->
		<div class="border-border flex shrink-0 flex-col gap-1 border-t p-2">
			<div class="flex items-center justify-between gap-0.5">
				<span class="text-subtle px-1 font-mono text-xs" title="Server version">
					{api.version ?? ''}
				</span>
				<ThemeToggle />
			</div>
			<AccountMenu />
		</div>
	</aside>
{/if}
