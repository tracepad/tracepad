import type { Score, ScoreConfig, ScoreInput } from './api/client.svelte';
import type { DataType } from './api/score-configs';
import { ABSENT } from './format';

// Everything about a score that is not a DOM node (spec 022): how a value
// reads for each of the four types, where a score says it came from, how one
// response splits between the trace header and the observation panels, and
// what the dialog sends.
//
// Pure and tested as a table, for the same reason `$lib/prompts.ts` is: these
// are the rules a reader of a trace acts on, and a rule that only exists
// inside a component can only be tested by drawing one.

/** Which target a score is being written against; exactly one shape is used. */
export type ScoreTarget = { trace_id?: string; observation_id?: string; session_id?: string };

/** How much of a `text` score a chip shows before the expand (#3). */
export const TEXT_PREVIEW = 120;

/** What a score with no `metadata.source` is called (#3). */
export const API_SOURCE = 'api';

/**
 * The value as the type says to read it (#3): a number to three significant
 * digits, a boolean as a word, a category as itself, and text cut to its
 * preview unless the chip is expanded. Three digits because a chip is read at
 * a glance and `0.8571428571` is not; the whole number is in the tooltip.
 */
export function scoreValue(score: Score, whole = false): string {
	switch (score.data_type) {
		case 'numeric':
			return score.value == null ? ABSENT : String(Number(score.value.toPrecision(3)));
		case 'boolean':
			return score.value == null ? ABSENT : score.value === 1 ? 'yes' : 'no';
		case 'categorical':
			return score.string_value || ABSENT;
		default: {
			const text = score.string_value ?? '';
			if (text === '') return ABSENT;
			if (whole || text.length <= TEXT_PREVIEW) return text;
			return `${text.slice(0, TEXT_PREVIEW)}…`;
		}
	}
}

/** True when the chip is showing less than the score carries (edge cases). */
export function isCut(score: Score): boolean {
	return score.data_type === 'text' && (score.string_value ?? '').length > TEXT_PREVIEW;
}

/**
 * Who wrote it (#3). `metadata.source` is what the SDK harness and this
 * spec's own dialog write; a score with none came from somewhere holding a
 * key, which is what *api* says. A non-string source is somebody's structured
 * metadata and not a word to print.
 */
export function scoreSource(score: Score): string {
	const source = (score.metadata as Record<string, unknown> | null | undefined)?.source;
	return typeof source === 'string' && source !== '' ? source : API_SOURCE;
}

/**
 * What the value's tooltip says about the name's declared range, or nothing
 * when the name has no config or the config bounds neither end.
 */
export function boundsLabel(config: ScoreConfig | undefined): string | undefined {
	if (!config) return undefined;
	const { min, max } = config;
	if (min != null && max != null) return `${min} … ${max}`;
	if (min != null) return `at least ${min}`;
	if (max != null) return `at most ${max}`;
	return undefined;
}

/** One header chip, and whether its observation is anywhere in this trace. */
export type HeaderScore = { score: Score; unknown: boolean };

/**
 * One `GET /scores?trace_id=` response split between the two surfaces it
 * feeds (#1): a score naming an observation belongs on that observation's
 * panel, everything else on the trace header, and the header says how many
 * went elsewhere.
 *
 * A score whose `observation_id` is in no observation of this trace — a late
 * span, or a wrong id — would otherwise be shown nowhere at all, so the header
 * takes it with a note. It is not counted among the ones on observations
 * (Decision 11): the count is an invitation to open panels, so it has to be
 * what is behind them, and this one is on screen already.
 */
export function splitScores(scores: Score[], known: ReadonlySet<string>) {
	const header: HeaderScore[] = [];
	const byObservation = new Map<string, Score[]>();
	let onObservations = 0;
	for (const score of scores) {
		const at = score.observation_id;
		if (!at) {
			header.push({ score, unknown: false });
			continue;
		}
		if (!known.has(at)) {
			header.push({ score, unknown: true });
			continue;
		}
		onObservations++;
		const seen = byObservation.get(at);
		if (seen) seen.push(score);
		else byObservation.set(at, [score]);
	}
	return { header, byObservation, onObservations };
}

/** Every observation id in a trace, for the split and for the tree's badge. */
export function observationIDs(
	nodes: { id: string; children?: unknown[] }[] | undefined
): Set<string> {
	const ids = new Set<string>();
	const walk = (list: typeof nodes) => {
		for (const node of list ?? []) {
			ids.add(node.id);
			walk(node.children as typeof nodes);
		}
	};
	walk(nodes);
	return ids;
}

// --- the dialog ---------------------------------------------------------
//
// The form is strings, the way an input carries them, and the body is built
// from it in one place (#4). The type comes from the picked config when there
// is one, so a range or a category list is typed once — in the config — and
// the control is derived from it rather than from a second declaration here.

/** The `name` select's value when the free-name path is taken (#4). */
export const OTHER = '';

export type ScoreForm = {
	/** The config whose name was picked, or `OTHER`. */
	picked: string;
	/** The free name, used on the `OTHER` path only. */
	name: string;
	/** The type, stated on the `OTHER` path; the config's otherwise. */
	dataType: DataType | '';
	/** `numeric` and `boolean` values, as the input carries them. */
	number: string;
	/** `categorical` and `text` values. */
	text: string;
	comment: string;
};

/**
 * A fresh form, opened on the first name the project declared. A project that
 * declared none opens on the free path, which is then the only path there is
 * (#4).
 */
export function emptyScoreForm(configs: ScoreConfig[] = []): ScoreForm {
	return {
		picked: configs[0]?.name ?? OTHER,
		name: '',
		dataType: '',
		number: '',
		text: '',
		comment: ''
	};
}

/**
 * Whether nothing has been said on this form yet. It is what makes it safe to
 * seed it a second time: the configs can land after the dialog is open, and a
 * form still showing the free-name path for a project that declared names is
 * worth correcting — a form somebody has started typing into is not (found in
 * review of PR #41).
 */
export function isPristine(form: ScoreForm): boolean {
	return (
		form.picked === OTHER &&
		form.name === '' &&
		form.dataType === '' &&
		form.number === '' &&
		form.text === '' &&
		form.comment === ''
	);
}

/** The config a form is bound by, or undefined on the free-name path. */
export function configOf(form: ScoreForm, configs: ScoreConfig[]): ScoreConfig | undefined {
	return form.picked === OTHER ? undefined : configs.find((one) => one.name === form.picked);
}

/** What kind of value the form is asking for; `''` until the free path says. */
export function typeOf(form: ScoreForm, configs: ScoreConfig[]): DataType | '' {
	return configOf(form, configs)?.data_type ?? (form.picked === OTHER ? form.dataType : '');
}

/** The name the score will be filed under. */
export function nameOf(form: ScoreForm): string {
	return form.picked === OTHER ? form.name.trim() : form.picked;
}

/**
 * An existing score back in the form's shape, so one dialog creates and edits
 * (#5). A name the project has a config for opens on that config; one it does
 * not opens on the free path with the name and type filled, because that is
 * what the score actually is.
 */
export function formOfScore(score: Score, configs: ScoreConfig[]): ScoreForm {
	const config = configs.find((one) => one.name === score.name);
	return {
		picked: config ? config.name : OTHER,
		name: score.name,
		dataType: score.data_type,
		number: score.value == null ? '' : String(score.value),
		text: score.string_value ?? '',
		comment: score.comment ?? ''
	};
}

/**
 * What a stored score is *about*, as a target. The absent ids stay absent:
 * the API refuses an empty `trace_id` rather than reading it as "no trace".
 */
export function targetOf(score: Score): ScoreTarget {
	const target: ScoreTarget = {};
	if (score.trace_id) target.trace_id = score.trace_id;
	if (score.observation_id) target.observation_id = score.observation_id;
	if (score.session_id) target.session_id = score.session_id;
	return target;
}

/**
 * Why the server would refuse this form, or `null`. It mirrors the rules
 * spec 003 already gives rather than inventing any: a name, a type on the free
 * path, and a value of the shape the type takes. The `400` stays the oracle —
 * a range, a category list and a config replaced under the dialog are all
 * still the server's to judge.
 */
export function scoreProblem(form: ScoreForm, configs: ScoreConfig[]): string | null {
	if (nameOf(form) === '') return 'A score needs a name.';
	const type = typeOf(form, configs);
	if (type === '') return 'Say what kind of value this name carries.';
	if (type === 'numeric') {
		if (form.number.trim() === '') return 'A numeric score needs a value.';
		if (!Number.isFinite(Number(form.number))) return 'The value is not a number.';
	}
	if (type === 'boolean' && form.number === '') return 'Say yes or no.';
	if (type === 'categorical' && form.text === '') return 'Pick one of the categories.';
	if (type === 'text' && form.text.trim() === '') return 'A text score needs something to say.';
	return null;
}

/**
 * The body of the POST (#4, #5). A new score is stamped `source: "web"` and
 * carries no `timestamp`, so it is received now — a judgement made now.
 *
 * An edit resends the score being corrected: its id, its own **target**, and
 * with it the `metadata` and `timestamp` it already had (Decision 10). A
 * re-POST replaces the row whole, so a field left out is a field deleted:
 * taking the target from the block that happened to draw the chip would move
 * a score off the observation it grades — the *unknown observation* chip is
 * on the trace header, and the trace header's target names no observation —
 * and would drop the second id of a score that carries a trace and a session
 * both, which is a score the API takes and `scores add` writes. None of the
 * three ids is a field the dialog shows, so none of them is the reader's to
 * change (found in review of PR #41).
 */
export function scoreBody(
	target: ScoreTarget,
	form: ScoreForm,
	configs: ScoreConfig[],
	editing: Score | null = null,
	/**
	 * What to write into `metadata`, when the caller has something to say
	 * about where the verdict came from. The annotation desk does
	 * (spec 024 #6): a score it writes says which queue and which annotator,
	 * on a new row and on a correction alike, because the reviewer *is* now
	 * the author. Left out, this is spec 022's own stamp on a new score and
	 * the row's own metadata on a correction.
	 */
	stamp?: Record<string, unknown>
): ScoreInput {
	const type = typeOf(form, configs);
	const body: ScoreInput = {
		...(editing ? targetOf(editing) : target),
		name: nameOf(form),
		// Stated rather than inferred, on every type: the inference of
		// spec 003 #5 would read a boolean as numeric and a category as text,
		// which is the one thing this dialog does know.
		data_type: type === '' ? undefined : type,
		comment: form.comment.trim()
	};
	if (type === 'numeric' || type === 'boolean') body.value = Number(form.number);
	else body.string_value = form.text;

	if (editing) {
		body.id = editing.id;
		if (stamp) body.metadata = stamp;
		else if (editing.metadata != null) body.metadata = editing.metadata;
		body.timestamp = editing.timestamp;
	} else {
		body.metadata = stamp ?? { source: 'web' };
	}
	return body;
}

/**
 * Which field the server's refusal is about, when its sentence names one
 * (Application contract). The messages are spec 003's own, and the point is
 * to put the reason beside the control that caused it; anything unmatched
 * stays at Save, where it is still read.
 */
export function refusedField(message: string): 'name' | 'value' | null {
	const text = message.toLowerCase();
	if (text.includes('"name"')) return 'name';
	if (/value|categor|"data_type"|is numeric|is boolean|is text|min|max/.test(text)) return 'value';
	return null;
}
