// What kind of step an observation was, and how the tree draws it (spec 012
// #2). One module, because the vocabulary, the label and the icon have to
// agree: a type the API can send and the tree cannot draw would render as a
// blank box in the one place a reader is scanning for shape.

import Bot from '@lucide/svelte/icons/bot';
import Box from '@lucide/svelte/icons/box';
import Gauge from '@lucide/svelte/icons/gauge';
import Layers from '@lucide/svelte/icons/layers';
import Link from '@lucide/svelte/icons/link';
import Search from '@lucide/svelte/icons/search';
import Shield from '@lucide/svelte/icons/shield';
import Sparkles from '@lucide/svelte/icons/sparkles';
import Wrench from '@lucide/svelte/icons/wrench';
import Zap from '@lucide/svelte/icons/zap';
import type { Component } from 'svelte';

/**
 * The ten values `observations.type` holds, in the order the schema's CHECK
 * lists them. A parity test reads `openapi.json` and fails if the API grew a
 * type this list does not have (the same idea as the filter parity test).
 */
export const OBSERVATION_TYPES = [
	'span',
	'generation',
	'event',
	'agent',
	'tool',
	'chain',
	'retriever',
	'guardrail',
	'evaluator',
	'embedding'
] as const;

export type ObservationType = (typeof OBSERVATION_TYPES)[number];

/**
 * The icon per kind (spec 012, Application contract). Icons rather than the
 * three-letter labels the tree used before: at ten values the labels stop
 * being scannable — `RETR`, `GUAR`, `EVAL` all read alike at a glance — and
 * the shape of a row is what somebody skimming a hundred spans is after.
 *
 * `Record<ObservationType, …>` is the parity: a type in the list above with no
 * icon here does not compile.
 */
export const TYPE_ICONS: Record<ObservationType, Component> = {
	span: Box,
	generation: Sparkles,
	event: Zap,
	agent: Bot,
	tool: Wrench,
	chain: Link,
	retriever: Search,
	guardrail: Shield,
	evaluator: Gauge,
	embedding: Layers
};

/** Whether the API sent a kind this build knows how to draw. */
export function isObservationType(value: string | undefined): value is ObservationType {
	return OBSERVATION_TYPES.includes(value as ObservationType);
}

/**
 * The icon for a kind, falling back to the plain span box. A server newer than
 * this build could send a type the icon map has never heard of; a box beside
 * the name it always shows is a better answer than a hole in the row.
 */
export function typeIcon(type: string | undefined): Component {
	return isObservationType(type) ? TYPE_ICONS[type] : TYPE_ICONS.span;
}

/**
 * What the icon's tooltip says. The kind is otherwise unreadable — an icon
 * with no name is a rebus — and this is also what a screen reader announces.
 */
export function typeLabel(type: string | undefined): string {
	return type ?? 'span';
}
