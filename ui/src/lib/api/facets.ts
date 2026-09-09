import type { Facets, FacetValue } from './client.svelte';

// The filter panel's own logic for a many-valued filter (spec 027 #6, #7):
// what the checkbox list holds, how the comma form travels in the URL, and how
// the chip reads. Pure, because that is what makes it worth testing without a
// DOM — and because the interesting failures here are silent ones: a value
// that arrived from a link and quietly vanished from the list is a filter
// nobody can undo.

/** The three filters that take a list and have a list to offer (Decision 6). */
export const FACET_FIELDS = ['environment', 'release', 'name'] as const;

export type FacetField = (typeof FACET_FIELDS)[number];

/** One row of the checkbox list. */
export type FacetOption = {
	value: string;
	/**
	 * How many traces of the range carry it, and `null` for a value that came
	 * from the URL and is not in the answer — a link to `?environment=canary`
	 * from before canary was retired still filters, and must still be
	 * undoable (Decision 6).
	 */
	count: number | null;
	checked: boolean;
};

/**
 * Above this many values the list gets a box that narrows it by substring. It
 * appears only when a list is long enough to need one, for the reason the
 * search box is not on every listing.
 */
export const FILTER_BOX_FROM = 8;

/** The comma form as an array: what the URL carries, as the panel holds it. */
export function readList(value: string | undefined): string[] {
	if (!value) return [];
	const seen = new Set<string>();
	for (const item of value.split(',')) {
		const trimmed = item.trim();
		if (trimmed) seen.add(trimmed);
	}
	return [...seen];
}

/**
 * And back: the comma form the URL, the chips and the API all spell the same
 * way (Decision 7). Empty is `undefined`, because a parameter with no value is
 * a 400 rather than "no filter".
 */
export function writeList(values: string[]): string | undefined {
	const list = values.map((value) => value.trim()).filter(Boolean);
	return list.length ? [...new Set(list)].join(',') : undefined;
}

/**
 * The rows to draw: the checked values the answer does not mention first,
 * then the answer's own list in the order it came — count descending, which is
 * what tells `prod: 1` from `production: 4656`.
 *
 * The pinned rows are the reason this function exists. A URL is a document: a
 * link to a value the range no longer holds must still show that it is
 * filtering, and unchecking it must be possible. They carry no count because
 * there is none to carry — nothing in the range has that value.
 */
export function facetOptions(
	values: FacetValue[],
	checked: string[],
	/**
	 * Which out-of-list values to pin. It defaults to the checked ones and is
	 * passed separately by the field, which remembers them: unchecking a
	 * pinned value must leave its box where it is, or the only control that
	 * could put it back vanishes under the cursor.
	 */
	pinned: string[] = checked
): FacetOption[] {
	const picked = new Set(checked);
	const known = new Set(values.map((one) => one.value));
	const extra = [...new Set(pinned)]
		.filter((value) => !known.has(value))
		.map((value) => ({ value, count: null, checked: picked.has(value) }));
	return [
		...extra,
		...values.map((one) => ({
			value: one.value,
			count: one.count,
			checked: picked.has(one.value)
		}))
	];
}

/**
 * The substring narrowing, case-folded because a person typing `PROD` means
 * `production`. A checked value stays whatever is typed: hiding it would hide
 * the only control that can uncheck it.
 */
export function narrowFacet(options: FacetOption[], query: string): FacetOption[] {
	const needle = query.trim().toLowerCase();
	if (!needle) return options;
	return options.filter((one) => one.checked || one.value.toLowerCase().includes(needle));
}

/**
 * How a many-valued chip reads (Decision 7): up to two values by name, and
 * beyond that the number, because the chip row is read at a glance and a
 * glance holds two names. The tooltip keeps the names one hover away.
 */
export function facetChip(label: string, values: string[]): { text: string; title: string } {
	const title = `${label}: ${values.join(', ')}`;
	if (values.length <= 2) return { text: title, title };
	return { text: `${label}: ${values.length} values`, title };
}

/** The three lists of one answer, keyed by the field they belong to. */
export function facetLists(answer: Facets): Record<FacetField, FacetValue[]> {
	return {
		environment: answer.environment,
		release: answer.release,
		name: answer.name
	};
}

/** And how many values the cap left out of each (spec 027 #2). */
export function facetOmitted(answer: Facets): Record<FacetField, number> {
	return {
		environment: answer.omitted.environment,
		release: answer.omitted.release,
		name: answer.omitted.name
	};
}
