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
import { within } from '$lib/paths';
import { SCREENS, type Screen } from '$lib/screens';

export type Item = {
	href: string;
	label: string;
	icon: Component<{ class?: string }>;
	/** A tab of its own on a phone rather than a row in *More* (spec 006 #20). */
	tab?: true;
};
/** A labelled group of items (spec 016 #1): the label is not a link. */
export type Group = { label: string; children: Item[] };

/** Each screen's icon: a screen added to `SCREENS` without one does not compile. */
const ICONS: Record<(typeof SCREENS)[number]['href'], Item['icon']> = {
	'/dashboard': LayoutDashboard,
	'/traces': ListTree,
	'/sessions': MessagesSquare,
	'/users': Users,
	'/prompts': ScrollText,
	'/datasets': Database,
	'/runs': FlaskConical,
	'/score-configs': Ruler,
	'/queues': ClipboardCheck,
	'/quality': ChartSpline,
	'/settings': Settings
};

export const isGroup = (section: Item | Group): section is Group => 'children' in section;

/**
 * Navigation: the list of screens (`screens.ts`) with their icons, the ones
 * filed under a label gathered into a group — a row too; the list nests once.
 * The four marked `tab` are a phone's tabs (spec 006 #20): the front page and
 * the three places a failure is read.
 */
export const SECTIONS: (Item | Group)[] = [];
for (const { href, label, ...rest } of SCREENS as readonly Screen[]) {
	const item: Item = { href, label, icon: ICONS[href as keyof typeof ICONS], ...(rest.tab && { tab: true }) };
	const last = SECTIONS.at(-1);
	if (!rest.group) SECTIONS.push(item);
	else if (last && isGroup(last) && last.label === rest.group) last.children.push(item);
	else SECTIONS.push({ label: rest.group, children: [item] });
}

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
