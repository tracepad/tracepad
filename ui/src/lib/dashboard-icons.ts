import CircleDollarSign from '@lucide/svelte/icons/circle-dollar-sign';
import Cpu from '@lucide/svelte/icons/cpu';
import Layers from '@lucide/svelte/icons/layers';
import Tag from '@lucide/svelte/icons/tag';
import TextInitial from '@lucide/svelte/icons/text-initial';
import Timer from '@lucide/svelte/icons/timer';
import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
import type { Component } from 'svelte';
import type { BlockId } from '$lib/dashboard';
import { ICONS } from '$lib/sections';

/**
 * The one icon of each idea on the dashboard (spec 034 #16): a tile, a chart
 * and a table that speak of the same thing wear the same glyph. Traces and
 * Quality are the sidebar's own, so a screen and its figure agree. Tokens are
 * letters, not coins — coins read as Cost.
 */
/** A block that has an icon: every one but the summary row, which is four tiles. */
export type IconBlockId = Exclude<BlockId, 'summary'>;

export const BLOCK_ICONS: Record<IconBlockId, Component<{ class?: string }>> = {
	traces: ICONS['/traces'],
	cost: CircleDollarSign,
	tokens: TextInitial,
	latency: Timer,
	errors: TriangleAlert,
	models: Cpu,
	environments: Layers,
	releases: Tag,
	quality: ICONS['/quality']
};
