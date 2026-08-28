// How numbers and instants are rendered everywhere in the app. One module,
// because a trace list whose latency column disagrees with the detail panel's
// is a bug nobody reports and everybody distrusts.
//
// Every formatter answers `em dash` for a value the server did not send.
// Absent is not zero: a trace whose client never priced it has no cost, and
// showing `$0.0000` would be a stored lie (the API is careful about this, and
// so is the screen).

/** What every formatter renders for a value that is not there. */
export const ABSENT = '—';

/**
 * Durations read left to right at a glance, so the unit changes with the
 * magnitude and the precision shrinks as the number grows: three significant
 * figures are plenty to compare two spans, and more of them only make the
 * column jitter.
 */
export function duration(ms: number | null | undefined): string {
	if (ms == null || !Number.isFinite(ms) || ms < 0) return ABSENT;
	if (ms < 1) return `${Math.round(ms * 1000)} µs`;
	if (ms < 1000) return `${round(ms, ms < 100 ? 1 : 0)} ms`;
	if (ms < 60_000) return `${round(ms / 1000, ms < 10_000 ? 2 : 1)} s`;
	const minutes = Math.floor(ms / 60_000);
	const seconds = Math.floor((ms % 60_000) / 1000);
	return `${minutes}m ${String(seconds).padStart(2, '0')}s`;
}

/**
 * Costs are compared across orders of magnitude — a cent and a hundredth of a
 * cent sit in the same column — so small amounts keep the digits that
 * distinguish them instead of rounding to `$0.00`.
 */
export function cost(usd: number | null | undefined): string {
	if (usd == null || !Number.isFinite(usd)) return ABSENT;
	if (usd === 0) return '$0';
	if (usd < 0.01) return `$${usd.toPrecision(2)}`;
	return `$${usd.toFixed(usd < 1 ? 4 : 2)}`;
}

/** Whole counts, grouped, so a five-figure token count is readable. */
export function count(value: number | null | undefined): string {
	if (value == null || !Number.isFinite(value)) return ABSENT;
	return value.toLocaleString('en-US');
}

/**
 * Payload sizes, as the truncation markers report them (spec 004 #2). The unit
 * is the one a person reads on a "load the full 240 KB" button, so it is
 * decimal and never fractional past one digit.
 */
export function bytes(size: number | null | undefined): string {
	if (size == null || !Number.isFinite(size) || size < 0) return ABSENT;
	if (size < 1000) return `${size} B`;
	if (size < 1_000_000) return `${round(size / 1000, size < 10_000 ? 1 : 0)} KB`;
	return `${round(size / 1_000_000, 1)} MB`;
}

/**
 * An absolute instant in the reader's own zone. Traces are correlated with
 * logs and dashboards that all speak local time; UTC here would make the
 * reader do the arithmetic on every row.
 */
export function timestamp(iso: string | null | undefined): string {
	const at = instant(iso);
	if (!at) return ABSENT;
	return at.toLocaleString(undefined, {
		month: 'short',
		day: '2-digit',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
		hour12: false
	});
}

/** The same instant with milliseconds, for the detail panel's timings. */
export function timestampPrecise(iso: string | null | undefined): string {
	const at = instant(iso);
	if (!at) return ABSENT;
	return `${timestamp(iso)}.${String(at.getMilliseconds()).padStart(3, '0')}`;
}

/** Parses an API timestamp, treating anything unparseable as absent. */
export function instant(iso: string | null | undefined): Date | null {
	if (!iso) return null;
	const at = new Date(iso);
	return Number.isNaN(at.getTime()) ? null : at;
}

/** Milliseconds between two API timestamps, or null if either is missing. */
export function elapsed(from: string | null | undefined, to: string | null | undefined): number | null {
	const start = instant(from);
	const end = instant(to);
	if (!start || !end) return null;
	return end.getTime() - start.getTime();
}

// `toFixed` keeps trailing zeros, which make a column look noisier than the
// data is; `Number()` drops them without turning 1e-7 into exponent notation
// at the sizes we format.
function round(value: number, digits: number): string {
	return String(Number(value.toFixed(digits)));
}
