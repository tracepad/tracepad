import { ABSENT } from './format';

/**
 * Below `md` — the width at which the sidebar becomes a top bar — a listing
 * keeps the columns that say which row it is and whether it went wrong, and
 * folds the rest into a line under the row (spec 006 #18). A component reads
 * it through `new MediaQuery(PHONE)` and renders one layout, never both with
 * one hidden: a hidden copy is still text that find-in-page, a screen reader
 * or a test lands on.
 */
export const PHONE = '(max-width: 47.99rem)';

/** The values of a folded line that say something, joined the way the line reads. */
export function folded(values: readonly (string | null | undefined)[]): string {
	return values.filter((one) => one && one !== ABSENT).join(' · ');
}
