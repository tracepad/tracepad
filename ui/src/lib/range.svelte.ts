import { matchPreset, presetRange, PRESETS, type PresetKey, type Range } from '$lib/api/range';
import { auth } from '$lib/auth.svelte';

// The time window, remembered in this browser (spec 034 #7). A preset is
// kept as its key and re-resolved against the clock when applied, so "the
// last 24 hours" still ends now a week later; a calendar range names days and
// is kept as the days. Per account, as the remembered project is (spec 028
// #13), so two people on one browser do not share a window. Nothing here
// goes to the server: the window changes many times a day, and a `PATCH` per
// change is traffic for a setting whose loss costs one click.

const STORAGE_PREFIX = 'tracepad.range.';

type Stored = { preset: PresetKey } | { from?: string; to?: string };

/**
 * The window this account last set, resolved against `now` — or null when
 * none is kept, storage is unreadable, or what is kept is not a window. The
 * screens that open on a default preset read this in place of the default;
 * a window in the URL wins over both.
 */
export function rememberedRange(now: Date): Range | null {
	const account = auth.account?.id;
	if (!account) return null;
	let stored: Stored;
	try {
		stored = JSON.parse(window.localStorage.getItem(STORAGE_PREFIX + account) ?? '');
	} catch {
		return null;
	}
	if (!stored || typeof stored !== 'object') return null;
	if ('preset' in stored) {
		return PRESETS.some((preset) => preset.key === stored.preset)
			? presetRange(stored.preset, now)
			: null;
	}
	const range: Range = {};
	for (const bound of ['from', 'to'] as const) {
		const value = stored[bound];
		if (typeof value !== 'string' || Number.isNaN(Date.parse(value))) continue;
		range[bound] = value;
	}
	return range.from || range.to ? range : null;
}

/**
 * Keeps a window the person just set: as the preset it matches, else as its
 * dates. Every screen with the range control calls this on a change, the
 * listings included, even though the listings never read it back.
 */
export function rememberRange(range: Range, now: Date) {
	const account = auth.account?.id;
	if (!account) return;
	const preset = matchPreset(range, now);
	const stored: Stored = preset ? { preset } : { from: range.from, to: range.to };
	try {
		if (!range.from && !range.to) window.localStorage.removeItem(STORAGE_PREFIX + account);
		else window.localStorage.setItem(STORAGE_PREFIX + account, JSON.stringify(stored));
	} catch {
		// Storage is a browser feature a person can switch off; the window
		// still applies, it just will not outlive the tab.
	}
}
