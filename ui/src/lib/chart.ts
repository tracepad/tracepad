// Which lines of a chart are drawn (spec 049 #9, #20). Pure, so the rule that
// keeps a window from drawing empty is a test and not a thing to try by hand.

export type Line = {
	label: string;
	values: (number | null)[];
	token: string;
	/** Drawn only once the legend entry is clicked; its live value shows either way. */
	hidden?: boolean;
};

export const hasData = (line: Line) => line.values.some((value) => value !== null);

/**
 * Whether `line` is drawn: the reader's choice from the legend, else the
 * caller's default. A line hidden by default has nothing to give when it is
 * the only one with data, and hiding it would draw an empty plot over a
 * window that is not empty — so then every line is drawn, choices included.
 */
export function drawn(lines: readonly Line[], chosen: ReadonlyMap<string, boolean>, line: Line): boolean {
	const wanted = (one: Line) => chosen.get(one.label) ?? !one.hidden;
	if (!lines.some((one) => wanted(one) && hasData(one))) return true;
	return wanted(line);
}
