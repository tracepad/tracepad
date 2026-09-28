import { ABSENT } from './format';

/**
 * Below `md` — the width at which the sidebar becomes a tab bar. Written the
 * way Tailwind writes `max-md`, so the two partition the width with no gap
 * between them. A component reads it through `new MediaQuery(PHONE)`; a
 * listing asks its own box instead (`Fold`, spec 006 #22).
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
