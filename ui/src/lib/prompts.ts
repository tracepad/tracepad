// The pure part of the Prompts screens (spec 021): the Save gate, the chip
// order, the diff painter and the two conversions between a stored body and
// the form that edits it. Here rather than in the components, for the reason
// spec 016 put `evals.ts` here: a rule with a table of cases behind it is a
// test that needs no DOM, and a rule the components share cannot drift.

import type { components } from './api/schema';

export type Prompt = components['schemas']['Prompt'];
export type PromptType = 'text' | 'chat';

/** The name grammar the server enforces (docs/prompts.md), mirrored. */
export const NAME_RULE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
export const NAME_MAX = 200;
/** The one label name the server reserves: it always names the highest version. */
export const RESERVED_LABEL = 'latest';

/** One message of a chat body, as the editor holds it. */
export type Message = { role: string; content: string };

/**
 * What the editor is editing. `name` is empty on the page where the name is
 * fixed and shown; the two bodies are both here because switching type must
 * not throw away what was typed under the other one.
 */
export type Draft = {
	name: string;
	type: PromptType;
	text: string;
	messages: Message[];
	config: string;
	commit: string;
	labels: string[];
};

export function emptyDraft(type: PromptType = 'chat'): Draft {
	return {
		name: '',
		type,
		text: '',
		messages: [{ role: 'system', content: '' }],
		config: '',
		commit: '',
		labels: []
	};
}

/**
 * The draft a version is edited from (Decision 4): an edit *is* a new version,
 * so the editor opens on a copy of the one being edited. `commit` starts empty
 * — a commit message describes the change about to be made, and inheriting the
 * previous one would file it under a sentence about something else.
 */
export function draftFrom(prompt: Prompt): Draft {
	const draft = emptyDraft(prompt.type as PromptType);
	if (prompt.type === 'text') {
		draft.text = typeof prompt.prompt === 'string' ? prompt.prompt : stringify(prompt.prompt);
	} else {
		draft.messages = messagesOf(prompt.prompt);
	}
	draft.config = prompt.config ? JSON.stringify(prompt.config, null, 2) : '';
	return draft;
}

/**
 * A chat body as messages. A role outside the datalist (`tool`, `function`) is
 * the string it is, and content that is not a string — the structured content
 * the API stores verbatim — is kept as its JSON so that saving preserves it
 * rather than flattening it to `[object Object]` (edge cases).
 */
export function messagesOf(body: unknown): Message[] {
	if (!Array.isArray(body)) return [{ role: 'system', content: '' }];
	return body.map((message) => {
		const one = (message ?? {}) as { role?: unknown; content?: unknown };
		return {
			role: typeof one.role === 'string' ? one.role : '',
			content: typeof one.content === 'string' ? one.content : stringify(one.content)
		};
	});
}

function stringify(value: unknown): string {
	return value === undefined || value === null ? '' : JSON.stringify(value, null, 2);
}

/**
 * Where the Save gate says no, by field (Decision 5). Every rule here is one
 * the server refuses with a `400` naming it: the check is mirrored so that a
 * person is told at the field rather than after a round trip, and the server
 * still decides.
 *
 * Keys are field ids so the editor can put each sentence under its own input:
 * `name`, `body`, `role:N`, `content:N`, `config`, `labels`.
 */
export function problems(draft: Draft, naming: boolean): Record<string, string> {
	const found: Record<string, string> = {};
	if (naming) {
		const name = draft.name.trim();
		if (name === '') found.name = 'A prompt needs a name.';
		else if (name.length > NAME_MAX) found.name = `At most ${NAME_MAX} characters.`;
		else if (!NAME_RULE.test(name)) {
			found.name = 'Letters, digits, dot, dash and underscore, starting with a letter or a digit.';
		}
	}
	if (draft.type === 'text') {
		if (draft.text.trim() === '') found.body = 'A text prompt needs a body.';
	} else {
		if (draft.messages.length === 0) found.body = 'A chat prompt needs at least one message.';
		draft.messages.forEach((message, i) => {
			if (message.role.trim() === '') found[`role:${i}`] = 'A message needs a role.';
			// Append-only: an empty message cannot be edited away later, so
			// the client that reads it back would find out at the model call.
			if (message.content.trim() === '') found[`content:${i}`] = 'A message needs content.';
		});
	}
	if (draft.config.trim() !== '' && !isObject(draft.config)) {
		found.config = 'Config is a JSON object, or nothing at all.';
	}
	if (draft.labels.some((label) => label === RESERVED_LABEL)) {
		found.labels = `“${RESERVED_LABEL}” is reserved: it always names the highest version.`;
	}
	const bad = draft.labels.find((label) => label !== RESERVED_LABEL && !NAME_RULE.test(label));
	if (bad && !found.labels) found.labels = `“${bad}” is not a label name.`;
	return found;
}

function isObject(text: string): boolean {
	try {
		const value: unknown = JSON.parse(text);
		return typeof value === 'object' && value !== null && !Array.isArray(value);
	} catch {
		return false;
	}
}

/** The version request a draft becomes; call it only on a draft with no problems. */
export function versionBody(draft: Draft) {
	const body: {
		type: PromptType;
		prompt: string | Message[];
		config?: Record<string, never>;
		commit_message?: string;
		labels?: string[];
	} = {
		type: draft.type,
		prompt:
			draft.type === 'text'
				? draft.text
				: draft.messages.map((message) => ({
						role: message.role.trim(),
						content: message.content
					}))
	};
	// `Record<string, never>` is what `openapi-typescript` makes of an object
	// with no declared properties — the config is whatever a runtime reads, and
	// the API states no shape for it.
	if (draft.config.trim() !== '') {
		body.config = JSON.parse(draft.config) as Record<string, never>;
	}
	if (draft.commit.trim() !== '') body.commit_message = draft.commit.trim();
	if (draft.labels.length > 0) body.labels = draft.labels;
	return body;
}

/**
 * Labels in the order they are shown: `production` first, then alphabetical
 * (Application contract). What is in production is the thing a reader scans a
 * column of chips for, and a strict alphabet buries it under `canary`.
 */
export function orderLabels<T extends string>(labels: readonly T[]): T[] {
	return [...labels].sort((a, b) => {
		if (a === b) return 0;
		if (a === 'production') return -1;
		if (b === 'production') return 1;
		return a < b ? -1 : 1;
	});
}

/** The listing's chips: the same order, each with the version it points at. */
export function orderLabelEntries(labels: Record<string, number>): [string, number][] {
	return orderLabels(Object.keys(labels)).map((label) => [label, labels[label]]);
}

/** One line of a rendered diff, classified by what a unified patch makes it. */
export type DiffLine = { kind: 'add' | 'remove' | 'hunk' | 'file' | 'context'; text: string };

/**
 * The server's patch, classified for painting (Decision 3). The interface
 * computes no diff — this reads the one `GET /prompts/{name}/diff` returned and
 * decides what colour each line is, which is rendering.
 *
 * The file headers are checked before the single-character prefixes: `---` and
 * `+++` start with the removal and addition markers, and painting them as a
 * removal and an addition would say the two names changed.
 */
export function paintDiff(diff: string): DiffLine[] {
	if (diff === '') return [];
	return diff.replace(/\n$/, '').split('\n').map(classify);
}

function classify(text: string): DiffLine {
	if (text.startsWith('---') || text.startsWith('+++')) return { kind: 'file', text };
	if (text.startsWith('@@')) return { kind: 'hunk', text };
	if (text.startsWith('+')) return { kind: 'add', text };
	if (text.startsWith('-')) return { kind: 'remove', text };
	return { kind: 'context', text };
}

/** `?diff=A..B` as two versions, or null when it names no pair. */
export function readDiff(raw: string | null): { from: number; to: number } | null {
	if (!raw) return null;
	const [from, to] = raw.split('..');
	const pair = { from: Number(from), to: Number(to) };
	const whole = (n: number) => Number.isInteger(n) && n >= 1;
	return whole(pair.from) && whole(pair.to) ? pair : null;
}

export const diffParam = (from: number, to: number) => `${from}..${to}`;
