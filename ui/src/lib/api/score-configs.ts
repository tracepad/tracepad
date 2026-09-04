import type { ScoreConfig, ScoreConfigInput } from './client.svelte';

// What a score name may mean, as the API spells it (spec 016 #13): the two
// vocabularies come from the generated schema rather than from a list typed a
// second time here, and the rule between them is spec 014 #16 mirrored
// client-side — so the form refuses what the server would refuse, before the
// round trip, with the server still the oracle.

export type DataType = ScoreConfig['data_type'];
export type Direction = NonNullable<ScoreConfig['direction']>;

/**
 * The labels the form shows, keyed by the value the API takes. A record, not
 * an array: a data type added to the schema and not to this file is a type
 * error here rather than an option the form silently lacks.
 */
const TYPE_LABELS: Record<DataType, string> = {
	numeric: 'numeric — a number, optionally bounded',
	boolean: 'boolean — 0 or 1',
	categorical: 'categorical — one of a list of words',
	text: 'text — free text, not judged'
};

const DIRECTION_LABELS: Record<Direction, string> = {
	higher: 'higher is better',
	lower: 'lower is better',
	none: 'neither — typed and bounded, not judged'
};

export const DATA_TYPES = Object.keys(TYPE_LABELS) as DataType[];
export const DIRECTIONS = Object.keys(DIRECTION_LABELS) as Direction[];
export const typeLabel = (value: DataType) => TYPE_LABELS[value];
export const directionLabel = (value: Direction) => DIRECTION_LABELS[value];

/** Which kinds of name are judged, and so must say which way is better. */
export const judged = (type: DataType) => type === 'numeric' || type === 'boolean';
/** Which kind takes bounds; only a number has a range. */
export const bounded = (type: DataType) => type === 'numeric';

/** The form's fields, all of them as they come out of an input. */
export type ConfigForm = {
	name: string;
	data_type: DataType;
	direction: Direction | '';
	min: string;
	max: string;
	/** One per line, the way a list is typed. */
	categories: string;
	description: string;
};

export function emptyForm(): ConfigForm {
	return { name: '', data_type: 'numeric', direction: 'higher', min: '', max: '', categories: '', description: '' };
}

/** A stored config back in the form's shape, so the same form edits it. */
export function formOf(config: ScoreConfig): ConfigForm {
	return {
		name: config.name,
		data_type: config.data_type,
		direction: config.direction ?? '',
		min: config.min == null ? '' : String(config.min),
		max: config.max == null ? '' : String(config.max),
		categories: (config.categories ?? []).join('\n'),
		description: config.description ?? ''
	};
}

/** The lines of the categories field, trimmed and without the blank ones. */
export function categoriesOf(text: string): string[] {
	return text
		.split('\n')
		.map((line) => line.trim())
		.filter((line) => line !== '');
}

/**
 * Why the server would refuse this form, in its own terms (spec 014 #16), or
 * `null` when it would not. The point is not to be the authority — the PUT
 * still is — but to answer before the round trip, and to say the same thing
 * the round trip would.
 */
export function configProblem(form: ConfigForm): string | null {
	if (form.name.trim() === '') return 'A score config needs a name.';
	if (judged(form.data_type) && form.direction === '') {
		return `A ${form.data_type} name is judged: say which direction is better.`;
	}
	if (!judged(form.data_type) && form.direction !== '') {
		return `A ${form.data_type} name has no direction.`;
	}
	if (!bounded(form.data_type) && (form.min !== '' || form.max !== '')) {
		return 'Only a numeric name takes bounds.';
	}
	for (const [label, raw] of [
		['minimum', form.min],
		['maximum', form.max]
	] as const) {
		if (raw !== '' && !Number.isFinite(Number(raw))) return `The ${label} is not a number.`;
	}
	if (form.min !== '' && form.max !== '' && Number(form.min) > Number(form.max)) {
		return 'The minimum is above the maximum.';
	}
	const categories = categoriesOf(form.categories);
	if (form.data_type === 'categorical') {
		if (categories.length === 0) return 'A categorical name needs its categories, one per line.';
		if (new Set(categories).size !== categories.length) return 'The categories repeat.';
	} else if (categories.length > 0) {
		return `A ${form.data_type} name has no categories.`;
	}
	return null;
}

/**
 * The body of the `PUT`. Declarative (spec 014 #17): every field the config
 * has, every time — and a field the type does not take is left out rather
 * than sent empty, because an absent `direction` and a `direction` of `none`
 * are two different declarations.
 */
export function configBody(form: ConfigForm): ScoreConfigInput {
	const body: ScoreConfigInput = { data_type: form.data_type };
	if (form.direction !== '') body.direction = form.direction;
	if (form.min !== '') body.min = Number(form.min);
	if (form.max !== '') body.max = Number(form.max);
	const categories = categoriesOf(form.categories);
	if (categories.length > 0) body.categories = categories;
	const description = form.description.trim();
	if (description !== '') body.description = description;
	return body;
}
