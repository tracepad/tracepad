// Tokens on screen (spec 049 #9): the one number a column shows, how it reads
// in a narrow cell, and the classes its tooltip lists. Pure, so every table
// reads a row's tokens the same way and a test pins the one rule that matters —
// no class is ever added to another (#1, #3).

import { ABSENT, count } from './format';
import type { components } from './api/schema';

export type Tokens = components['schemas']['Tokens'];

/**
 * Input plus output — what a bill is made of — with a missing class read as
 * zero, and null when neither was reported (#3). Cache read, reasoning and
 * cache write are never added in: providers disagree about whether they are
 * inside the input and the output already.
 */
export function billedTokens(tokens: Tokens | null | undefined): number | null {
	if (!tokens || (tokens.input == null && tokens.output == null)) return null;
	return (tokens.input ?? 0) + (tokens.output ?? 0);
}

/** A count as a column cell holds it: `950`, `12.4k`, `3.1M` (#9). */
export function compact(value: number | null | undefined): string {
	if (value == null || !Number.isFinite(value)) return ABSENT;
	return new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 })
		.format(value)
		.replace('K', 'k');
}

const CLASSES: readonly [keyof Tokens, string][] = [
	['input', 'Input'],
	['output', 'Output'],
	['cache_read', 'Cache read'],
	['reasoning', 'Reasoning'],
	['cache_write', 'Cache write']
];

/**
 * Every class that was reported, one per line, exact — the column's tooltip.
 * Undefined when nothing was, so the cell carries no empty tooltip.
 */
export function tokenClasses(tokens: Tokens | null | undefined): string | undefined {
	if (!tokens) return undefined;
	const lines = CLASSES.filter(([key]) => tokens[key] != null).map(
		([key, label]) => `${label} ${count(tokens[key])}`
	);
	return lines.length > 0 ? lines.join('\n') : undefined;
}
