import ChartSpline from '@lucide/svelte/icons/chart-spline';
import ClipboardCheck from '@lucide/svelte/icons/clipboard-check';
import Database from '@lucide/svelte/icons/database';
import FlaskConical from '@lucide/svelte/icons/flask-conical';
import LayoutDashboard from '@lucide/svelte/icons/layout-dashboard';
import ListTree from '@lucide/svelte/icons/list-tree';
import MessagesSquare from '@lucide/svelte/icons/messages-square';
import Ruler from '@lucide/svelte/icons/ruler';
import ScrollText from '@lucide/svelte/icons/scroll-text';
import Settings from '@lucide/svelte/icons/settings';
import Users from '@lucide/svelte/icons/users';
import type { Component } from 'svelte';
import { page } from '$app/state';
import { within } from '$lib/project.svelte';

export type Item = {
	href: string;
	label: string;
	icon: Component<{ class?: string }>;
	/** A tab of its own on a phone rather than a row in *More* (spec 006 #20). */
	tab?: true;
};
/** A labelled group of items (spec 016 #1): the label is not a link. */
export type Group = { label: string; children: Item[] };

/**
 * Navigation as data: spec 007 added three screens by adding three rows,
 * and spec 016 adds its first *section* — three screens that are one topic,
 * grouped under a label so a seven-item column says what four of them have
 * in common (#1). A group is a row too; the list nests once. The four marked
 * `tab` are a phone's tabs (spec 006 #20): the front page and the three
 * places a failure is read.
 */
export const SECTIONS: (Item | Group)[] = [
	// First, because it is the door (spec 034 #1): the screen that was
	// Stats, seventh, is the project's front page.
	{ href: '/dashboard', label: 'Dashboard', icon: LayoutDashboard, tab: true },
	{ href: '/traces', label: 'Traces', icon: ListTree, tab: true },
	{ href: '/sessions', label: 'Sessions', icon: MessagesSquare, tab: true },
	// Between the two screens it joins (spec 023 #8): a user is a set of
	// sessions, and the user page is the dashboard for one of them.
	{ href: '/users', label: 'Users', icon: Users, tab: true },
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

export const isGroup = (section: Item | Group): section is Group => 'children' in section;

/**
 * The active screen: the one whose path this URL is under, compared after
 * the project prefix (spec 029 #2) — every section lives under `/p/{id}`, and
 * the id is not part of which screen this is.
 */
export const active = (path: string) => within(page.url.pathname).startsWith(path);

/** Every destination, groups opened, in the order the column lists them. */
export const ITEMS: Item[] = SECTIONS.flatMap((section) =>
	isGroup(section) ? section.children : [section]
);

/** A phone's tabs (spec 006 #20), in the column's order. */
export const TABS: Item[] = ITEMS.filter((item) => item.tab);

/** What a phone's *More* holds: the column without its tabs, the group kept. */
export const MORE: (Item | Group)[] = SECTIONS.flatMap((section): (Item | Group)[] => {
	if (!isGroup(section)) return section.tab ? [] : [section];
	return [{ ...section, children: section.children.filter((child) => !child.tab) }];
});
