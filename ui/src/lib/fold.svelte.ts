import { MediaQuery } from 'svelte/reactivity';
import { PHONE } from './phone';

/**
 * Whether a listing folds (spec 006 #18, #22): it keeps the columns that say
 * which row it is and whether it went wrong, and folds the rest into lines
 * under the row, whenever the box it scrolls in is narrower than `whole` —
 * the width its table takes unfolded, given to the table as its `min-width`,
 * so that below it the table would have scrolled. That is a phone, and just
 * as much a desktop listing beside the sidebar or inside the peek panel.
 *
 * A component binds the box's `clientWidth` to `box`, sets the table's
 * `min-width` to `min` and renders one layout by `narrow` — never both with
 * one hidden: a hidden copy is still text that find-in-page, a screen reader
 * or a test lands on. Until the box is measured, and in a box with no layout,
 * which measures 0, the viewport answers instead, so a phone never draws the
 * wide table first.
 */
export class Fold {
	box = $state<number>();
	readonly whole: number;
	#phone = new MediaQuery(PHONE);

	constructor(whole: number) {
		this.whole = whole;
	}

	get narrow(): boolean {
		return this.box ? this.box < this.whole : this.#phone.current;
	}

	/** The table's `min-width`: its whole width unfolded, nothing folded. */
	get min(): string | undefined {
		return this.narrow ? undefined : `${this.whole}px`;
	}
}
