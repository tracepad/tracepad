import { describe, expect, it } from 'vitest';
import {
	CUSTOM_ROLE,
	defaultDiff,
	diffCeiling,
	diffParam,
	diffable,
	draftFrom,
	emptyDraft,
	messagesOf,
	orderLabelEntries,
	orderLabels,
	paintDiff,
	pickRole,
	problems,
	promptSearch,
	readDiff,
	roleAfter,
	versionBody,
	type Draft,
	type Message,
	type Prompt
} from './prompts';

// The rules of the Prompts screens, checked where they live (spec 021,
// Testing): the Save gate per rule, the chip order, the diff painter's
// classification and the two conversions between a stored body and the form.

const chat = (messages: Message[]): Draft => ({
	...emptyDraft('chat'),
	messages
});

describe('the Save gate', () => {
	it('passes a chat draft with one whole message', () => {
		expect(problems(chat([{ role: 'system', content: 'Be terse.' }]), false)).toEqual({});
	});

	it('names the rule at the field a message breaks', () => {
		const found = problems(
			chat([
				{ role: '', content: 'Be terse.' },
				{ role: 'user', content: '   ' }
			]),
			false
		);
		expect(found['role:0']).toMatch(/role/i);
		expect(found['content:1']).toMatch(/content/i);
		// And not the other way round: the second message's role is fine.
		expect(found['role:1']).toBeUndefined();
		expect(found['content:0']).toBeUndefined();
	});

	it('refuses a chat prompt with no messages at all', () => {
		expect(problems(chat([]), false).body).toMatch(/at least one message/i);
	});

	it('refuses an empty text body and accepts a filled one', () => {
		const empty: Draft = { ...emptyDraft('text'), text: '  ' };
		expect(problems(empty, false).body).toMatch(/body/i);
		expect(problems({ ...empty, text: 'Be terse.' }, false)).toEqual({});
	});

	// The server refuses `latest` with a `400` naming it (spec 003 #11), and a
	// form that lets somebody reach that 400 has not done its job.
	it('refuses `latest` as a label and takes any other name', () => {
		const draft = chat([{ role: 'system', content: 'Be terse.' }]);
		expect(problems({ ...draft, labels: ['production', 'latest'] }, false).labels).toMatch(
			/reserved/i
		);
		expect(problems({ ...draft, labels: ['production', 'canary-2'] }, false)).toEqual({});
		expect(problems({ ...draft, labels: ['-nope'] }, false).labels).toMatch(/not a label name/i);
	});

	it('takes an empty config and refuses one that is not an object', () => {
		const draft = chat([{ role: 'system', content: 'Be terse.' }]);
		expect(problems({ ...draft, config: '  ' }, false)).toEqual({});
		expect(problems({ ...draft, config: '{"model": "claude"}' }, false)).toEqual({});
		expect(problems({ ...draft, config: '{' }, false).config).toMatch(/JSON object/i);
		// An array parses and is not an object, which is what the server says.
		expect(problems({ ...draft, config: '[1]' }, false).config).toMatch(/JSON object/i);
	});

	// The name is asked for on the page that creates one, and nowhere else: a
	// name is fixed for the life of the prompt.
	it('checks the name only where a name is being chosen', () => {
		const draft = chat([{ role: 'system', content: 'Be terse.' }]);
		expect(problems(draft, false).name).toBeUndefined();
		expect(problems(draft, true).name).toMatch(/needs a name/i);
		expect(problems({ ...draft, name: '-leading' }, true).name).toMatch(/letters, digits/i);
		expect(problems({ ...draft, name: 'a'.repeat(201) }, true).name).toMatch(/200/);
		expect(problems({ ...draft, name: 'support.answer_v2-b' }, true)).toEqual({});
	});
});

// The role is a select of the roles the runtimes name, with a *Custom…* entry
// for the rest (spec 021 #16). What is saved is a string either way — `custom`
// is form state, and the body never carries it.
describe('the role of a message', () => {
	it('alternates from the role of the message before it', () => {
		expect(roleAfter([{ role: 'system', content: '' }])).toBe('user');
		expect(roleAfter([{ role: 'user', content: '' }])).toBe('assistant');
		expect(roleAfter([{ role: 'assistant', content: '' }])).toBe('user');
		// Anything else — a role that is not a turn, a custom one, or no
		// message at all — opens a `user`; the other five are one click away.
		expect(roleAfter([{ role: 'tool', content: '' }])).toBe('user');
		expect(roleAfter([{ role: 'critic', content: '', custom: true }])).toBe('user');
		expect(roleAfter([])).toBe('user');
	});

	it('opens a role outside the select in custom mode, and the six in it', () => {
		expect(messagesOf([{ role: 'function', content: 'called' }])).toEqual([
			{ role: 'function', content: 'called', custom: true }
		]);
		// `tool` and `model` are in the select now, so they are not custom.
		expect(messagesOf([{ role: 'tool', content: 'ok' }])).toEqual([
			{ role: 'tool', content: 'ok' }
		]);
		// A message that arrives with no role at all is a custom one waiting to
		// be typed, which is what the Save gate then says at the field.
		expect(messagesOf([{ content: 'orphan' }])).toEqual([
			{ role: '', content: 'orphan', custom: true }
		]);
	});

	it('empties the role when Custom… is picked, and fills it when a role is', () => {
		const message = { role: 'assistant', content: 'Answering.' };
		const custom = pickRole(message, CUSTOM_ROLE);
		expect(custom).toEqual({ role: '', content: 'Answering.', custom: true });
		// The Save gate is the one of #5: a message needs a role, custom or not.
		expect(problems(chat([custom]), false)['role:0']).toMatch(/role/i);
		expect(pickRole(custom, 'developer')).toEqual({
			role: 'developer',
			content: 'Answering.',
			custom: false
		});
	});

	it('sends a custom role as the string it is', () => {
		const draft = chat([{ role: ' function ', content: 'called', custom: true }]);
		expect(problems(draft, false)).toEqual({});
		expect(versionBody(draft).prompt).toEqual([{ role: 'function', content: 'called' }]);
	});

	// The `system` a fresh draft opens with is a prefill, and nothing asks for
	// one: a single `user` turn is a prompt like any other (#16).
	it('takes a body that holds no system message', () => {
		const draft = chat([
			{ role: 'user', content: 'Summarize this.' },
			{ role: 'assistant', content: 'Sure.' }
		]);
		expect(problems(draft, false)).toEqual({});
		expect(versionBody(draft).prompt).toEqual([
			{ role: 'user', content: 'Summarize this.' },
			{ role: 'assistant', content: 'Sure.' }
		]);
	});
});

describe('the version a draft becomes', () => {
	it('sends the body, the config, the message and the labels', () => {
		const draft: Draft = {
			...chat([{ role: ' system ', content: 'Be terse.' }]),
			config: '{"temperature": 0.2}',
			commit: '  tighten tone  ',
			labels: ['production']
		};
		expect(versionBody(draft)).toEqual({
			type: 'chat',
			prompt: [{ role: 'system', content: 'Be terse.' }],
			config: { temperature: 0.2 },
			commit_message: 'tighten tone',
			labels: ['production']
		});
	});

	// An absent field and an empty one mean the same thing to the API, and the
	// request says what it means rather than sending empties.
	it('leaves out what was not filled in', () => {
		expect(versionBody({ ...emptyDraft('text'), text: 'Be terse.' })).toEqual({
			type: 'text',
			prompt: 'Be terse.'
		});
	});

	// The API stores any JSON value as a message's content and returns it
	// verbatim (spec 003 #15). The editor is a text area, so a structured
	// content is *shown* as its document — and opening a prompt and saving it
	// untouched must give the array back, not a string of it (found in review
	// of PR #40).
	it('puts a structured content back as the value it was rendered from', () => {
		const parts = [{ type: 'text', text: 'hi' }];
		const draft = { ...emptyDraft('chat'), messages: messagesOf([{ role: 'user', content: parts }]) };

		expect(versionBody(draft)).toEqual({
			type: 'chat',
			prompt: [{ role: 'user', content: parts }]
		});
	});

	// Once it is edited, the text is what the author wrote: guessing at its
	// structure would be the editor having an opinion about their document.
	it('sends the text once the author has changed it', () => {
		const draft = {
			...emptyDraft('chat'),
			messages: messagesOf([{ role: 'user', content: [{ type: 'text', text: 'hi' }] }])
		};
		draft.messages[0].content = 'just words now';

		expect(versionBody(draft).prompt).toEqual([{ role: 'user', content: 'just words now' }]);
	});

	// The ordinary case is untouched: a string content stays a string, and
	// nothing is parsed on the way out.
	it('leaves a plain string content alone, whatever it looks like', () => {
		const draft = {
			...emptyDraft('chat'),
			messages: messagesOf([{ role: 'user', content: '{"looks": "like JSON"}' }])
		};

		expect(versionBody(draft).prompt).toEqual([{ role: 'user', content: '{"looks": "like JSON"}' }]);
	});
});

describe('the label chips', () => {
	// `production` is what a reader scans a column of chips for; a strict
	// alphabet buries it under `canary`.
	it('puts production first and sorts the rest', () => {
		expect(orderLabels(['staging', 'canary', 'production', 'a'])).toEqual([
			'production',
			'a',
			'canary',
			'staging'
		]);
		expect(orderLabels(['staging', 'canary'])).toEqual(['canary', 'staging']);
		expect(orderLabels([])).toEqual([]);
	});

	it('keeps the version each label points at', () => {
		expect(orderLabelEntries({ staging: 9, production: 3 })).toEqual([
			['production', 3],
			['staging', 9]
		]);
	});
});

describe('the diff painter', () => {
	const diff = [
		'--- prompt (version 1)',
		'+++ prompt (version 2)',
		'@@ -1 +1 @@',
		'-"Be brief."',
		'+"Be brief and cite the source."',
		' unchanged',
		''
	].join('\n');

	// The file headers start with the removal and the addition markers, so a
	// painter that only looked at the first character would say the two names
	// changed.
	it('classifies every kind of line, headers before prefixes', () => {
		expect(paintDiff(diff).map((line) => line.kind)).toEqual([
			'file',
			'file',
			'hunk',
			'remove',
			'add',
			'context'
		]);
	});

	it('keeps the text as the server sent it', () => {
		expect(paintDiff(diff)[3].text).toBe('-"Be brief."');
	});

	// `?diff=3..3` is a real question with a short answer (edge cases).
	it('reads an empty diff as no lines at all', () => {
		expect(paintDiff('')).toEqual([]);
	});
});

describe("the prompt page's URL", () => {
	const url = (search: string) => new URLSearchParams(search);

	it('leaves the listing keys where they are', () => {
		expect(promptSearch(url('limit=25&cursor=abc&direction=prev'), { version: 4 })).toBe(
			'?limit=25&cursor=abc&direction=prev&version=4'
		);
	});

	// The whole of finding (2): opening a diff is about the diff, and the
	// version being read is not its business.
	it('keeps the version being read when it is not told about one', () => {
		expect(promptSearch(url('version=1'), { diff: '1..2' })).toBe('?version=1&diff=1..2');
		expect(promptSearch(url('version=1&diff=1..2'))).toBe('?version=1');
		// And with no version in the URL there is still none: the page reads
		// the latest, and says so by saying nothing.
		expect(promptSearch(url(''), { diff: '1..2' })).toBe('?diff=1..2');
	});

	it('sets a version when given one and drops it when given null', () => {
		expect(promptSearch(url('version=1&diff=1..2'), { version: 3 })).toBe('?version=3');
		expect(promptSearch(url('version=1'), { version: null })).toBe('');
	});

	// A diff is never carried by accident: every link that is not about it
	// leaves it behind.
	it('drops the diff unless it is the thing being set', () => {
		expect(promptSearch(url('diff=1..2'), { version: 5 })).toBe('?version=5');
		expect(promptSearch(url('diff=1..2'))).toBe('');
	});
});

describe('what the page may claim to know', () => {
	const rows = (...versions: number[]) => versions.map((version) => ({ version }));

	// Finding (3): the listing is newest first, so only its first page carries
	// the highest version. Page two of a long history caps at a page boundary.
	it('bounds the diff inputs only on the newest page', () => {
		expect(diffCeiling(rows(120, 119, 118), true)).toBe(120);
		expect(diffCeiling(rows(70, 69, 68), false)).toBeUndefined();
		expect(diffCeiling([], true)).toBeUndefined();
	});

	// Finding (5): a listing in flight and a listing that failed both look
	// like "one version" from the rows alone.
	it.each([
		['a name with one version', { loading: false, problem: null, rows: 1, newest: true }, false],
		['a name with two', { loading: false, problem: null, rows: 2, newest: true }, true],
		['a later page', { loading: false, problem: null, rows: 1, newest: false }, true],
		['a page in flight', { loading: true, problem: null, rows: 0, newest: true }, true],
		['a page that failed', { loading: false, problem: 'nope', rows: 0, newest: true }, true]
	])('says %s is comparable: %o → %s', (_, listing, want) => {
		expect(diffable(listing)).toBe(want);
	});
});

describe('the ?diff= parameter', () => {
	it('reads a pair and refuses everything else', () => {
		expect(readDiff('1..2')).toEqual({ from: 1, to: 2 });
		expect(readDiff('3..3')).toEqual({ from: 3, to: 3 });
		expect(readDiff(null)).toBeNull();
		expect(readDiff('')).toBeNull();
		expect(readDiff('1')).toBeNull();
		expect(readDiff('0..2')).toBeNull();
		expect(readDiff('a..b')).toBeNull();
		expect(readDiff('1.5..2')).toBeNull();
	});

	it('writes what it reads', () => {
		expect(readDiff(diffParam(4, 7))).toEqual({ from: 4, to: 7 });
	});

	// `1..1` is a comparison of a thing with itself, and *Diff* answering
	// "identical" on v1 of a six-version name is a dead button (found in
	// review of PR #40). Versions are gapless, so v2 exists whenever v1 is
	// not the only one — and the button is shut when it is.
	it('opens on the version before, or on the one after when there is none', () => {
		expect(defaultDiff(7)).toEqual({ from: 6, to: 7 });
		expect(defaultDiff(2)).toEqual({ from: 1, to: 2 });
		expect(defaultDiff(1)).toEqual({ from: 1, to: 2 });
	});
});

describe('the draft a version opens as', () => {
	const version = (extra: Partial<Prompt>): Prompt => ({
		name: 'support',
		version: 2,
		type: 'chat',
		prompt: [{ role: 'system', content: 'Be terse.' }],
		labels: [],
		created_at: '2026-09-07T10:00:00Z',
		...extra
	});

	it('copies the body and the config, and starts the message empty', () => {
		// `config` is a free-form object; `openapi-typescript` renders "an
		// object with no declared properties" as `Record<string, never>`.
		const config = { model: 'claude' } as unknown as Prompt['config'];
		const draft = draftFrom(version({ config, commit_message: 'earlier' }));
		expect(draft.messages).toEqual([{ role: 'system', content: 'Be terse.' }]);
		expect(JSON.parse(draft.config)).toEqual({ model: 'claude' });
		// A commit message describes the change about to be made.
		expect(draft.commit).toBe('');
		expect(draft.labels).toEqual([]);
	});

	it('opens a text prompt as its text, not as JSON', () => {
		const draft = draftFrom(version({ type: 'text', prompt: 'Be terse.' }));
		expect(draft.type).toBe('text');
		expect(draft.text).toBe('Be terse.');
	});

	// A role outside the datalist and a structured content are both stored
	// verbatim by the API, so the editor keeps them rather than flattening them
	// (edge cases).
	it('keeps a role it does not know and content that is not a string', () => {
		const parts = [{ type: 'text', text: 'hi' }];
		expect(messagesOf([{ role: 'tool', content: parts }])).toEqual([
			{
				role: 'tool',
				content: '[\n  {\n    "type": "text",\n    "text": "hi"\n  }\n]',
				// What the document was rendered from, so a save can put it back.
				structured: parts
			}
		]);
	});
});
