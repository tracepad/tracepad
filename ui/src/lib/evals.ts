// The pure part of the Evals screens (spec 016): the rules a table applies to
// what the API sent and nothing the API could have said itself. Every number a
// screen shows — a mean, a verdict, a delta — comes from the server (spec 014
// #18); what lives here is which rows to draw, and how to say a thing.

import type { ComparedItem, Run } from '$lib/api/client.svelte';

/**
 * Whether two ticked runs can be compared, and if not, why (spec 016 #11).
 * Disabled with the reason rather than letting the server 400: runs of
 * different datasets are refused there (spec 014 #18), and a mistake should
 * stay a tooltip rather than become a page.
 */
export function compareChoice(chosen: readonly Pick<Run, 'id' | 'dataset'>[]): {
	enabled: boolean;
	reason: string;
} {
	if (chosen.length < 2) return { enabled: false, reason: 'Tick two runs to compare them' };
	if (chosen.length > 2) return { enabled: false, reason: 'Compare takes exactly two runs' };
	const [a, b] = chosen;
	if (a.dataset !== b.dataset) {
		return { enabled: false, reason: 'Runs of different datasets cannot be compared' };
	}
	return { enabled: true, reason: `Compare ${short(a.id)} with ${short(b.id)}` };
}

/** Where a comparison of two runs lives; the order is the order they were ticked in. */
export function compareHref(a: string, b: string): string {
	return `/runs/${encodeURIComponent(a)}/compare/${encodeURIComponent(b)}`;
}

/**
 * The rows the *changed only* toggle keeps (spec 016 #10): a row goes only
 * when every verdict it carries says `same`. A row with no verdict at all — a
 * case one run answered and the other did not — stays, because it is exactly
 * the row somebody looking for what changed wants to see.
 */
export function changedOnly(items: readonly ComparedItem[]): ComparedItem[] {
	return items.filter((item) => {
		const verdicts = Object.values(item.scores);
		return verdicts.length === 0 || verdicts.some((score) => score.verdict !== 'same');
	});
}

/**
 * The id the run's item view gives the group of traces that named the run and
 * no item at all (spec 014 #29) — `null` on the wire, which a listing keyed by
 * id and a `?peek=` in the URL both need a word for.
 */
export const NO_ITEM = 'none';

/**
 * The walk key of a row of a run's item view: the dataset's items by `seq`,
 * then the unknown groups after them, which is the order the server lists
 * them in (spec 014, `RunItems`). Fixed width, so the keys compare as text.
 */
export function runItemKey(row: { seq?: number | null; id: string }): string {
	return row.seq == null ? `~${row.id}` : String(row.seq).padStart(12, '0');
}

/** The first characters of a value as compact JSON, for a table cell. */
export function preview(value: unknown, width = 120): string {
	if (value === undefined || value === null) return '';
	const text = typeof value === 'string' ? value : JSON.stringify(value);
	const chars = [...text];
	return chars.length > width ? chars.slice(0, width - 1).join('') + '…' : text;
}

/** The first eight characters, which is how an id reads in a column. */
export function short(id: string): string {
	return id.slice(0, 8);
}

/**
 * How long a run has been open, beside `running` (spec 014 #8, spec 016 #7):
 * an honest "the harness has not said", with the one number that helps decide
 * whether it still will.
 */
export function age(since: string | null | undefined, now = Date.now()): string {
	if (!since) return '';
	const ms = now - new Date(since).getTime();
	if (!Number.isFinite(ms) || ms < 0) return '';
	const minutes = Math.floor(ms / 60_000);
	if (minutes < 1) return 'just now';
	if (minutes < 60) return `${minutes} min`;
	const hours = Math.floor(minutes / 60);
	if (hours < 48) return `${hours} h ${minutes % 60} min`;
	return `${Math.floor(hours / 24)} d`;
}

/** A score value as the API sent it, for a chip: a number, a word, or absence. */
export function scoreText(value: unknown): string {
	if (value === null || value === undefined) return '—';
	if (typeof value === 'number') return trim(value);
	return String(value);
}

/** A signed delta, which is the number somebody quotes. */
export function deltaText(delta: number | null | undefined): string {
	if (delta === null || delta === undefined) return '—';
	return delta > 0 ? `+${trim(delta)}` : trim(delta);
}

/** A number the way a person writes it: no trailing zeros, four places at most. */
export function trim(value: number): string {
	return String(Number(value.toFixed(4)));
}
