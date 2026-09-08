import type { AnnotationQueue, Score, ScoreConfig, ScoreInput } from './api/client.svelte';
import { count } from './format';
import { OTHER, formOfScore, scoreBody, scoreProblem, typeOf, type ScoreForm } from './scores';

// Everything about an annotation queue that is not a DOM node (spec 024): how
// far a queue has got, what the New-queue form refuses, the form the desk
// builds from the scores a queue asks for, and what *Complete* posts.
//
// Pure and tested as a table, for the reason `$lib/scores.ts` is: these are
// the rules a reviewer acts on, and a rule that only exists inside a component
// can only be tested by drawing one.

/** How many traces one *Add to queue…* may take (the endpoint's own cap). */
export const FROM_TRACES_CAP = 1000;

/** The name grammar the server holds a queue to (#1). */
const NAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

/**
 * Why the server would refuse this New-queue form, or `null`. It mirrors the
 * rules the API already gives rather than inventing any: the `400` stays the
 * oracle, and a name this project already has is a question only the server
 * can answer.
 */
export function queueProblem(name: string, configs: string[]): string | null {
	const trimmed = name.trim();
	if (trimmed === '') return 'A queue needs a name.';
	if (trimmed.length > 200) return 'A name is at most 200 characters.';
	if (!NAME.test(trimmed)) {
		return 'A name starts with a letter or a digit, then letters, digits, dots, dashes or underscores.';
	}
	if (configs.length === 0) return 'Pick at least one score: a queue is the scores it asks for.';
	return null;
}

/** How far a queue has got, as the bar and the desk header read it (#10). */
export function progress(queue: AnnotationQueue) {
	const { pending, completed, skipped } = queue.counts;
	const total = pending + completed + skipped;
	const share = (part: number) => (total === 0 ? 0 : (part / total) * 100);
	return {
		total,
		completed,
		skipped,
		/** `12 of 40`, which is what the desk's header says. */
		label: `${count(completed)} of ${count(total)}`,
		completedPercent: share(completed),
		skippedPercent: share(skipped)
	};
}

/**
 * One control on the desk: the score the queue asks for, the config that says
 * what it means, the form the reviewer is filling, and the score already on
 * the target — which is what makes the form *prefilled* rather than empty
 * (#12).
 */
export type DeskField = {
	name: string;
	config: ScoreConfig | undefined;
	form: ScoreForm;
	/** The score already on the target under this name, when there is one. */
	existing: Score | null;
	/**
	 * The id a *new* score of this name will be written under, minted when the
	 * field is built and stable for as long as it is on screen.
	 *
	 * It is what makes saving retry-safe (found in review). The desk posts the
	 * scores and then completes, and the completion can fail — the shape was
	 * not filled, somebody else got there first, the network went. Pressing
	 * *Complete & next* again re-posts, and without an id `POST /api/v1/scores`
	 * mints one per call: the second attempt wrote a *second* `accuracy` row on
	 * the same trace, and every mean over that name counted the verdict twice.
	 * With an id the re-post is the upsert spec 003 #3 designed it to be.
	 */
	newID: string;
};

/**
 * The desk's form: one field per config the queue names, in the queue's own
 * order, prefilled from the scores already on the target.
 *
 * Prefilling is Decision 7's "whoever wrote it" made visible: a judge's
 * verdict already on the trace is a verdict, and the reviewer confirms or
 * edits it rather than repeating it.
 *
 * A name whose config was deleted since keeps its place (edge cases): the
 * queue still asks for it, so the desk asks for it too — as a free-typed
 * score of that name, which is exactly what the API would then accept.
 */
export function deskFields(
	names: string[],
	configs: ScoreConfig[],
	scores: Score[]
): DeskField[] {
	return names.map((name) => {
		const config = configs.find((one) => one.name === name);
		const existing = scores.find((score) => score.name === name) ?? null;
		return {
			name,
			config,
			existing,
			newID: newScoreID(),
			form: existing
				? formOfScore(existing, configs)
				: {
						picked: config ? name : OTHER,
						name,
						dataType: config ? config.data_type : 'text',
						number: '',
						text: '',
						comment: ''
					}
		};
	});
}

/**
 * A score id of the shape the API mints for itself: 32 lower-case hex
 * characters (spec 003 #3). Minted here so that a re-post is a correction of
 * the row this desk already wrote rather than a second one.
 */
function newScoreID(): string {
	const bytes = new Uint8Array(16);
	crypto.getRandomValues(bytes);
	return [...bytes].map((byte) => byte.toString(16).padStart(2, '0')).join('');
}

/**
 * The names still missing a value, which is Decision 7's rule applied here
 * rather than instead of there: the server checks the stored scores and this
 * checks the form, so *Complete* is not offered for a round trip that would
 * be refused. The `409` is still the oracle, and its `missing` marks the
 * controls when the two disagree.
 */
export function unfilled(fields: DeskField[], configs: ScoreConfig[]): string[] {
	return fields.filter((field) => scoreProblem(field.form, configs) !== null).map((f) => f.name);
}

/**
 * Whether this field says something the stored score does not. Saving posts
 * what changed and nothing else (#12): a queue over a target somebody else
 * already scored can be completed without writing a thing.
 */
export function changed(field: DeskField, configs: ScoreConfig[]): boolean {
	if (scoreProblem(field.form, configs) !== null) return false;
	const stored = field.existing;
	if (!stored) return true;
	// The same split `scoreBody` makes: a number or a boolean rides in
	// `value`, a category or a body of text in `string_value`.
	const type = typeOf(field.form, configs);
	const numeric = type === 'numeric' || type === 'boolean';
	return (
		(numeric ? Number(field.form.number) : null) !== (stored.value ?? null) ||
		(numeric ? null : field.form.text) !== (stored.string_value ?? null) ||
		field.form.comment.trim() !== (stored.comment ?? '')
	);
}

/**
 * What the desk posts for one field: spec 022's own body, stamped with where
 * the verdict came from (#6). `source: "annotation"` beside spec 022's
 * `"web"` keeps the chip on the trace honest about which surface wrote it,
 * and the queue and the annotator are how "who said this" is read back.
 */
export function deskBody(
	field: DeskField,
	configs: ScoreConfig[],
	target: { trace_id?: string; observation_id?: string },
	by: { queue: string; annotator: string }
): ScoreInput {
	const body = scoreBody(target, field.form, configs, field.existing, {
		source: 'annotation',
		queue: by.queue,
		annotator: by.annotator
	});
	// A correction carries the row's own id; a new score carries the one this
	// field was built with, so posting it twice writes one row.
	body.id = field.existing ? field.existing.id : field.newID;
	return body;
}

/**
 * What the traces listing's *Add to queue…* may do with the filters in force
 * (#13). The count is the one the listing already holds, so the reader sees
 * how many they are about to queue *before* the call — and a filter matching
 * more than the endpoint's cap is refused with the reason rather than
 * silently truncated to the newest thousand.
 */
export function queueable(total: { value: number; capped: boolean } | null) {
	if (total === null) return { label: 'the traces on screen', blocked: null };
	if (total.capped) {
		return {
			label: `${count(total.value)}+ traces`,
			blocked: `More than ${count(FROM_TRACES_CAP)} traces match. Narrow the filters — a queue is a list somebody has to work through.`
		};
	}
	return { label: `${count(total.value)} traces`, blocked: null };
}
