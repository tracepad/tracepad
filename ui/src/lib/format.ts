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
 * A wait — time to first token — formatted like a duration but keeping its
 * sign. `duration` renders a negative as absent, and it is right to: a
 * negative latency is a corrupt row. A negative TTFT is not. The client said
 * its first token came back before its span began, the API stores that as
 * sent and documents the sign, and a screen that renders it as `—` reports a
 * disagreeing clock as a missing measurement (spec 012 edge cases, found in
 * review of PR #19).
 */
export function wait(ms: number | null | undefined): string {
	if (ms == null || !Number.isFinite(ms)) return ABSENT;
	return ms < 0 ? `-${duration(-ms)}` : duration(ms);
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

/**
 * A long identifier cut in the middle rather than at the end. Applied to user
 * ids (spec 023, edge cases), which are the one identifier in this interface a
 * customer chooses the shape of: `customer_9f2a…:prod` and `customer_9f2a…:eu`
 * are one cell apart, and an end-truncated column renders both as the same
 * string. The whole of it is the cell's `title` and one copy button away.
 */
export function middleEllipsis(value: string, max = 28): string {
	if (value.length <= max) return value;
	// The head carries the shape of the id and the tail carries what usually
	// distinguishes two of them, so the split leans towards the front.
	const head = Math.ceil((max - 1) / 2);
	return `${value.slice(0, head)}…${value.slice(value.length - (max - 1 - head))}`;
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

// One formatter, not one per call: a score block draws a dozen of these and
// building an Intl formatter is the expensive half of the work.
const AGO = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });

const UNITS: [seconds: number, unit: Intl.RelativeTimeFormatUnit][] = [
	[31_536_000, 'year'],
	[2_592_000, 'month'],
	[86_400, 'day'],
	[3_600, 'hour'],
	[60, 'minute'],
	[1, 'second']
];

/**
 * How long ago, in the coarsest unit that still says something — *3 hours
 * ago*, *yesterday*. It is what a judgement wants beside it: whether a verdict
 * is from this run or from last month is the question, and the exact instant
 * is one hover away (spec 022 #3).
 *
 * `now` is a parameter so that the rendering is testable without a clock.
 */
export function relative(iso: string | null | undefined, now = Date.now()): string {
	const at = instant(iso);
	if (!at) return ABSENT;
	const seconds = Math.round((now - at.getTime()) / 1000);
	const size = Math.abs(seconds);
	const [span, unit] = UNITS.find(([each]) => size >= each) ?? [1, 'second'];
	// Negative is the past, which is what an API timestamp almost always is;
	// a clock ahead of the browser's reads as "in 2 minutes" rather than as a
	// missing value.
	return AGO.format(-Math.trunc(seconds / span), unit);
}

/** The same instant with milliseconds, for the detail panel's timings. */
/**
 * An instant that is null until something first happens — a sign-in, a key's
 * first use — as "never" rather than a blank, which would read as missing.
 */
export function timeOrNever(iso: string | null | undefined): string {
	return iso ? timestamp(iso) : 'never';
}

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
