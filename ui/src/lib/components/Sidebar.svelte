<script lang="ts" module>
	import ChartLine from '@lucide/svelte/icons/chart-line';
	import ChartSpline from '@lucide/svelte/icons/chart-spline';
	import ClipboardCheck from '@lucide/svelte/icons/clipboard-check';
	import Database from '@lucide/svelte/icons/database';
	import FlaskConical from '@lucide/svelte/icons/flask-conical';
	import ListTree from '@lucide/svelte/icons/list-tree';
	import MessagesSquare from '@lucide/svelte/icons/messages-square';
	import Ruler from '@lucide/svelte/icons/ruler';
	import ScrollText from '@lucide/svelte/icons/scroll-text';
	import Settings from '@lucide/svelte/icons/settings';
	import Users from '@lucide/svelte/icons/users';
	import type { Component } from 'svelte';

	type Item = { href: string; label: string; icon: Component<{ class?: string }> };
	/** A labelled group of items (spec 016 #1): the label is not a link. */
	type Group = { label: string; children: Item[] };

	/**
	 * Navigation as data: spec 007 added three screens by adding three rows,
	 * and spec 016 adds its first *section* — three screens that are one topic,
	 * grouped under a label so a seven-item column says what four of them have
	 * in common (#1). A group is a row too; the list nests once.
	 */
	export const SECTIONS: (Item | Group)[] = [
		{ href: '/traces', label: 'Traces', icon: ListTree },
		{ href: '/sessions', label: 'Sessions', icon: MessagesSquare },
		// Between the two screens it joins (spec 023 #8): a user is a set of
		// sessions, and the user page is Stats for one of them.
		{ href: '/users', label: 'Users', icon: Users },
		{ href: '/stats', label: 'Stats', icon: ChartLine },
		// Top level, not under *Evals* (spec 021 #1): a prompt is what the
		// application ships, and filing it under the test loop would say it
		// belongs to the eval nouns.
		{ href: '/prompts', label: 'Prompts', icon: ScrollText },
		{
			label: 'Evals',
			children: [
				{ href: '/datasets', label: 'Datasets', icon: Database },
				{ href: '/runs', label: 'Runs', icon: FlaskConical },
				{ href: '/score-configs', label: 'Score configs', icon: Ruler },
				// Fourth, and last (spec 024 #10): a queue is an eval noun —
				// the design lists it beside datasets and runs — and it is
				// what the section was made to hold.
				{ href: '/queues', label: 'Queues', icon: ClipboardCheck },
				// Fifth, after the queue (spec 025 #9): quality is what evals
				// produce, so it sits with the datasets, the runs and the
				// annotation desk rather than with the traffic on Stats.
				{ href: '/quality', label: 'Quality', icon: ChartSpline }
			]
		},
		{ href: '/settings', label: 'Settings', icon: Settings }
	];

	const isGroup = (section: Item | Group): section is Group => 'children' in section;
</script>

<script lang="ts">
	import { page } from '$app/state';
	import { api } from '$lib/api/client.svelte';
	import { href, switcher, within } from '$lib/project.svelte';
	import AccountMenu from './AccountMenu.svelte';
	import ProjectSwitcher from './ProjectSwitcher.svelte';
	import ThemeToggle from './ThemeToggle.svelte';

	// One markup, two shapes (spec 006 #15): a column down the left on a
	// desktop, a single bar across the top on a phone. Utilities rather than a
	// second component — the parts and their order are the same either way.

	/**
	 * The active screen: the one whose path this URL is under, compared
	 * after the project prefix (spec 029 #2) — every section lives under
	 * `/p/{id}`, and the id is not part of which screen this is.
	 */
	const active = (path: string) => within(page.url.pathname).startsWith(path);

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
			class={[link, indented && 'md:ml-3', active(entry.href) ? lit : dim]}
		>
			<Glyph class="size-4 shrink-0" />
			{entry.label}
		</a>
	</li>
{/snippet}

<!-- One markup, two shapes (spec 006 #15). On a phone it wraps into two rows —
     who you are, then where you can go — because the sections and a project
     name do not fit across 375 px; the wrapper's `md:contents` dissolves that
     second row so the desktop column is exactly what it was. DOM order matches
     the visual order in both, which is what keeps tabbing predictable. -->
<aside
	class="border-border bg-surface flex shrink-0 flex-wrap items-center gap-2 border-b px-3 py-2
		md:h-full md:w-52 md:flex-col md:flex-nowrap md:items-stretch md:gap-0 md:border-r
		md:border-b-0 md:p-0"
>
	<div class="flex shrink-0 items-center md:h-12 md:px-3">
		<span class="text-lg font-semibold tracking-tight">Tracepad</span>
	</div>

	<!-- The switcher (spec 029 #5), in the space the project name had: the
	     same control on the phone bar and in the column. -->
	<div class="flex min-w-0 flex-1 items-center md:flex-none md:px-2 md:pb-2">
		<ProjectSwitcher bind:open={switcher.open} />
	</div>

	<div class="flex w-full min-w-0 items-center gap-2 md:contents">
		<nav class="min-w-0 flex-1 md:px-2" aria-label="Sections">
			<!-- The sections wrap rather than scroll: a destination pushed off the
			     end of a scrollable strip is a destination nobody finds. On the
			     phone the group's children sit in the same wrapped row as their
			     siblings; the label is a word among them, not a heading over a
			     drawer (#1). -->
			<ul class="flex flex-wrap items-center gap-1 md:block">
				{#each SECTIONS as section (section.label)}
					{#if isGroup(section)}
						<li class="contents md:block">
							<span
								class="text-subtle px-2 py-1 text-xs font-medium tracking-wide uppercase md:mt-2 md:block"
							>
								{section.label}
							</span>
							<ul class="contents md:block">
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

		<!-- Who is signed in sits at the bottom of the column and at the end of
		     the phone's bar (spec 028 #14): the last thing on the way out, and
		     the one control that is about the reader rather than the data. -->
		<div
			class="border-border flex shrink-0 items-center gap-0.5
				md:mt-auto md:flex-col md:items-stretch md:gap-1 md:border-t md:px-2 md:py-2"
		>
			<div class="flex items-center gap-0.5 md:justify-between">
				<span class="text-subtle hidden px-1 font-mono text-xs md:inline" title="Server version">
					{api.version ?? ''}
				</span>
				<ThemeToggle />
			</div>
			<AccountMenu />
		</div>
	</div>
</aside>
