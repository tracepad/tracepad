import { ABSENT } from './format';

/**
 * Below `md` — the width at which the sidebar becomes a tab bar — a listing
 * keeps the columns that say which row it is and whether it went wrong, and
 * folds the rest into lines under the row (spec 006 #18). Written the way
 * Tailwind writes `max-md`, so the two partition the width with no gap
 * between them. A component reads it through `new MediaQuery(PHONE)` and
 * renders one layout, never both with one hidden: a hidden copy is still text
 * that find-in-page, a screen reader or a test lands on.
 */
export const PHONE = '(width < 48rem)';

/**
 * A reader who asked for less motion: a state change is an instant swap for
 * them rather than a slower one (spec 006 #11). Read through `MediaQuery`,
 * like `PHONE`, so every component asks the same question.
 */
export const STILL = '(prefers-reduced-motion: reduce)';

/** The values of a folded line that say something, in the order given. */
export function folded(values: readonly (string | null | undefined)[]): string[] {
	return values.filter((one): one is string => !!one && one !== ABSENT);
}
