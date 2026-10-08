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

/**
 * The indices of `values` that no line reaches: a value whose neighbours on
 * both sides are gaps, or the end of the series (spec 034 #14). uPlot joins
 * two adjacent values and nothing else, so a lone value surrounded by gaps is
 * drawn as nothing at all unless it is drawn as a point — and a week of hours
 * with one busy hour in it is exactly that.
 */
export function lonely(values: readonly (number | null)[]): number[] {
	const gap = (index: number) => index < 0 || index >= values.length || values[index] === null;
	return values.flatMap((value, index) =>
		value !== null && gap(index - 1) && gap(index + 1) ? [index] : []
	);
}
