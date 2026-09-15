import type { Account } from '$lib/api/client.svelte';

// The dashboard's blocks and how an account arranges them (spec 034 #8, #9).
// Pure: the arrangement is read out of an opaque preferences object and
// written back into it, and the interesting failures are silent — a block a
// newer build added must appear, a block this build does not know must
// survive the round trip, and a key another screen keeps must not be dropped.

/** Every block, in the default order; `wide` spans the grid. */
export const BLOCKS = [
	{ id: 'summary', label: 'Summary', wide: true },
	{ id: 'traces', label: 'Traces', wide: false },
	{ id: 'cost', label: 'Cost', wide: false },
	{ id: 'tokens', label: 'Tokens', wide: false },
	{ id: 'latency', label: 'Latency', wide: false },
	{ id: 'errors', label: 'Errors', wide: false },
	{ id: 'models', label: 'By model', wide: false },
	{ id: 'environments', label: 'By environment', wide: false },
	{ id: 'releases', label: 'By release', wide: false },
	{ id: 'quality', label: 'Quality', wide: true }
] as const;

export type BlockId = (typeof BLOCKS)[number]['id'];

/** The five charts, which one timeline request serves. */
export const CHARTS: readonly BlockId[] = ['traces', 'cost', 'tokens', 'latency', 'errors'];

/** The order every block is drawn in, and which of them are hidden. */
export type Arrangement = { order: BlockId[]; hidden: BlockId[] };

export const DEFAULT_ARRANGEMENT: Arrangement = {
	order: BLOCKS.map((block) => block.id),
	hidden: []
};

/** What the account keeps: the shape of `account.preferences`. */
export type Preferences = Account['preferences'];

/** Where the dashboard keeps its arrangements, keyed by project id. */
const KEY = 'dashboard';

export const label = (id: BlockId) => BLOCKS.find((block) => block.id === id)?.label ?? id;

const isBlock = (value: unknown): value is BlockId =>
	typeof value === 'string' && BLOCKS.some((block) => block.id === value);

/**
 * The arrangement stored for a project, made whole: unknown ids are
 * ignored, duplicates collapsed, and blocks missing from `order` appended in
 * default order — so a block added by a later spec appears without a
 * migration, and a value a newer build wrote is read without a crash.
 */
export function arrangementOf(preferences: Preferences, projectId: string): Arrangement {
	const stored = read(preferences, projectId);
	const order = [...new Set((stored?.order ?? []).filter(isBlock))];
	for (const block of BLOCKS) if (!order.includes(block.id)) order.push(block.id);
	const hidden = [...new Set((stored?.hidden ?? []).filter(isBlock))];
	return { order, hidden };
}

/**
 * The preferences object with this project's arrangement replaced, or
 * removed when `arrangement` is null (Reset). Everything else in the object
 * is carried as it was: the server replaces the object whole, so what is not
 * copied here is lost.
 */
export function withArrangement(
	preferences: Preferences,
	projectId: string,
	arrangement: Arrangement | null
): Preferences {
	const dashboards = { ...(asObject(preferences[KEY]) ?? {}) };
	if (arrangement) dashboards[projectId] = { order: arrangement.order, hidden: arrangement.hidden };
	else delete dashboards[projectId];
	return { ...preferences, [KEY]: dashboards };
}

/**
 * The order after the blocks on screen were dragged into `visible`: the
 * hidden blocks keep their slots, so one brought back reappears where it
 * was rather than at the end.
 */
export function reordered(arrangement: Arrangement, visible: BlockId[]): Arrangement {
	const queue = [...visible];
	const order = arrangement.order.map((id) =>
		arrangement.hidden.includes(id) ? id : (queue.shift() ?? id)
	);
	return { order, hidden: arrangement.hidden };
}

function read(preferences: Preferences, projectId: string): { order?: unknown[]; hidden?: unknown[] } | null {
	const stored = asObject(asObject(preferences[KEY])?.[projectId]);
	if (!stored) return null;
	return {
		order: Array.isArray(stored.order) ? stored.order : undefined,
		hidden: Array.isArray(stored.hidden) ? stored.hidden : undefined
	};
}

function asObject(value: unknown): Record<string, unknown> | null {
	return value && typeof value === 'object' && !Array.isArray(value)
		? (value as Record<string, unknown>)
		: null;
}
