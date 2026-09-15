// The time window every screen filters by, and the bucket the statistics are
// grouped into. One module, because the API speaks one half-open RFC 3339
// range everywhere (spec 007 #7) and a second interpretation of "last 24
// hours" living on another screen is how two screens start disagreeing about
// what they show.
//
// Everything here is pure and takes `now` as an argument: a clock read inside
// these functions would be untestable, and a window is a fact about an instant
// rather than about the moment it is rendered.

/** A window as the API takes it: `from` inclusive, `to` exclusive, both optional. */
export type Range = { from?: string; to?: string };

/**
 * The shortcuts the range control offers. A preset sets `from` only and leaves
 * `to` open, which is what "the last hour" means: the window keeps ending now,
 * so the refresh control genuinely refreshes rather than re-reading a frozen
 * slice of the past.
 */
export const PRESETS = [
	{ key: '1h', label: 'Last hour', ms: 3_600_000 },
	{ key: '24h', label: 'Last 24 hours', ms: 86_400_000 },
	{ key: '7d', label: 'Last 7 days', ms: 7 * 86_400_000 },
	{ key: '30d', label: 'Last 30 days', ms: 30 * 86_400_000 }
] as const;

export type PresetKey = (typeof PRESETS)[number]['key'];

/** What Stats opens on when the URL names no window (spec 007 #7). */
export const DEFAULT_PRESET: PresetKey = '7d';

/** Resolves a shortcut against the clock. */
export function presetRange(key: PresetKey, now: Date): Range {
	const preset = PRESETS.find((candidate) => candidate.key === key);
	if (!preset) return {};
	return { from: new Date(now.getTime() - preset.ms).toISOString() };
}

/** Reads the window out of a URL, ignoring anything that is not a timestamp. */
export function readRange(params: URLSearchParams): Range {
	const range: Range = {};
	for (const bound of ['from', 'to'] as const) {
		const value = params.get(bound)?.trim();
		if (value && !Number.isNaN(new Date(value).getTime())) range[bound] = value;
	}
	return range;
}

/**
 * How close to a preset's own boundary a window has to be to still be called
 * by its name. A minute: the URL carries the instant the preset resolved to,
 * and by the time the page renders it, that instant is a second or two old.
 */
const PRESET_TOLERANCE_MS = 60_000;

/**
 * Which shortcut a window came from, if any. A window with an explicit `to` is
 * never a preset: those always run up to now.
 */
export function matchPreset(range: Range, now: Date): PresetKey | null {
	if (!range.from || range.to) return null;
	const age = now.getTime() - new Date(range.from).getTime();
	if (Number.isNaN(age)) return null;
	const match = PRESETS.find((preset) => Math.abs(age - preset.ms) <= PRESET_TOLERANCE_MS);
	return match?.key ?? null;
}

/** The bucket granularities `GET /api/v1/stats` groups a timeline into. */
export const BUCKETS = ['hour', 'day'] as const;
export type Bucket = (typeof BUCKETS)[number];

/**
 * Where the automatic bucket switches over (spec 007 #6): up to two days of
 * hours is 48 points, which reads as a shape; past that the hours crowd into
 * noise and a day is the unit somebody is actually comparing.
 */
export const HOURLY_LIMIT_MS = 48 * 3_600_000;

/** The bucket a window gets when nobody has chosen one. */
export function defaultBucket(range: Range, now: Date): Bucket {
	return spanMs(range, now) <= HOURLY_LIMIT_MS ? 'hour' : 'day';
}

/** The chosen bucket, or the automatic one when the URL names nothing valid. */
export function readBucket(params: URLSearchParams, range: Range, now: Date): Bucket {
	const asked = params.get('group_by');
	return BUCKETS.includes(asked as Bucket) ? (asked as Bucket) : defaultBucket(range, now);
}

/** How long a window is, in milliseconds. An open start is unbounded. */
export function spanMs(range: Range, now: Date): number {
	if (!range.from) return Number.POSITIVE_INFINITY;
	const from = new Date(range.from).getTime();
	const to = range.to ? new Date(range.to).getTime() : now.getTime();
	if (Number.isNaN(from) || Number.isNaN(to)) return Number.POSITIVE_INFINITY;
	return to - from;
}

/**
 * The window of the same length ending where this one begins (spec 034 #2):
 * `from − (to − from)` to `from`, with `to` read as now for an open window.
 * Null when the window has no start, because then there is nothing before
 * it to compare against.
 */
export function previousRange(range: Range, now: Date): Range | null {
	if (!range.from) return null;
	const from = new Date(range.from).getTime();
	const to = range.to ? new Date(range.to).getTime() : now.getTime();
	if (Number.isNaN(from) || Number.isNaN(to) || to <= from) return null;
	return { from: new Date(from - (to - from)).toISOString(), to: new Date(from).toISOString() };
}
