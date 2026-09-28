import { MediaQuery } from 'svelte/reactivity';
import { PHONE } from './phone';

/** The px a rem is at the browser's default size, which the widths below are written in. */
const REM = 16;

/**
 * Whether a listing folds (spec 006 #18, #22): it keeps the columns that say
 * which row it is and whether it went wrong, and folds the rest into lines
 * under the row, whenever the box it scrolls in is narrower than `whole` —
 * the width its table takes unfolded, given to the table as its `min-width`,
 * so that below it the table would have scrolled. That is a phone, and just
 * as much a desktop listing beside the sidebar or inside the peek panel.
 *
 * `whole` is written in px at the default size and meant in rem, because the
 * columns are: at a reader's larger default the table takes more px, and so
 * does the line it folds at. A table whose columns come from the data gives a
 * function of it.
 *
 * A component binds the box's `contentRect` to `rect`, sets the table's
 * `min-width` to `min` and renders one layout by `narrow` — never both with
 * one hidden: a hidden copy is still text that find-in-page, a screen reader
 * or a test lands on. The content box is what is observed, not the border
 * box `clientWidth` follows: a scrollbar that appears in the box takes from
 * the first and not from the second, so the second would not report it.
 * Until the box is measured, and in a box with no layout, which measures 0,
 * the viewport answers instead, so a phone never draws the wide table first.
 */
export class Fold {
	rect = $state<DOMRectReadOnly>();
	#whole: () => number;
	#phone = new MediaQuery(PHONE);

	constructor(whole: number | (() => number)) {
		this.#whole = typeof whole === 'number' ? () => whole : whole;
	}

	get whole(): number {
		return this.#whole();
	}

	/** Worked out once per change of the box, not once per read: it asks the document how big a rem is. */
	narrow = $derived.by(() => {
		const box = this.rect?.width;
		return box ? box < (this.whole / REM) * root() : this.#phone.current;
	});

	/** The table's `min-width`: its whole width unfolded, nothing folded. */
	min = $derived(this.narrow ? undefined : `${this.whole / REM}rem`);
}

/** The size of a rem here, in px; a document that has not laid out reads the default. */
function root(): number {
	return parseFloat(getComputedStyle(document.documentElement).fontSize) || REM;
}
