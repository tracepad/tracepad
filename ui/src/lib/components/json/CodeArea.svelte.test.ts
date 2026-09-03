import { undo } from '@codemirror/commands';
import { foldedRanges } from '@codemirror/language';
import { forEachDiagnostic, forceLinting } from '@codemirror/lint';
import { EditorView } from '@codemirror/view';
import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { flushSync } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import JsonEditor from './JsonEditor.svelte';
import JsonView from './JsonView.svelte';
import { FOLD_OVER_LINES, asDocument, errorPosition, format, problem } from './setup';

// One surface reads and writes every payload (spec 015 #1). What is asserted
// here is the document rather than the pixels: CodeMirror draws the lines it
// can see, so `state.doc` and the fold set are what is true of a payload and
// the DOM is only what happens to be on screen at this height.

/** The instance behind a named region, the way CodeMirror finds one itself. */
function editor(label: string): EditorView {
	const found = EditorView.findFromDOM(screen.getByLabelText(label) as HTMLElement);
	if (!found) throw new Error(`no editor under ${label}`);
	return found;
}

const text = (label: string) => editor(label).state.doc.toString();
const folds = (label: string) => foldedRanges(editor(label).state).size;

/** The frame the document is folded on, a paint after it is mounted (#4). */
const painted = () => new Promise((wake) => requestAnimationFrame(wake));

/**
 * The pause the linter answers in. It is the component's only parse, so this
 * is also what `valid` waits for — one answer for a burst of typing rather
 * than one per keystroke.
 */
async function linted(label: string) {
	forceLinting(editor(label));
	// `linter` schedules its source on an idle callback; one turn of the
	// microtask queue plus a macrotask is what it takes to come back.
	await new Promise((wake) => setTimeout(wake, 0));
}

/** Where the linter put its marks, once it has actually run. */
async function marks(label: string): Promise<{ from: number; to: number }[]> {
	await linted(label);
	const found: { from: number; to: number }[] = [];
	forEachDiagnostic(editor(label).state, (_, from, to) => found.push({ from, to }));
	return found;
}

describe('a value becoming a document', () => {
	it('round-trips through the viewer at two-space indentation', () => {
		const value = { role: 'user', tokens: 12, nested: { deep: [1, 2] } };
		render(JsonView, { value, label: 'Input' });

		expect(text('Input')).toBe(JSON.stringify(value, null, 2));
		expect(JSON.parse(text('Input'))).toEqual(value);
	});

	it('shows a bare string as itself, with no JSON language (#12)', () => {
		const prompt = 'Summarise the release notes.\nKeep it to one line.';
		render(JsonView, { value: prompt, label: 'Input' });

		// Not `"Summarise…\nKeep…"`: quoting it to make it JSON would put an
		// escape on every newline of the thing the reader came to read.
		expect(text('Input')).toBe(prompt);
		expect(asDocument(prompt).plain).toBe(true);
		expect(asDocument({ a: 1 }).plain).toBe(false);
	});
});

describe('how much of a long document opens', () => {
	/**
	 * `entries` top-level keys, each four containers deep and branching in
	 * two at the bottom — so that folding one level too deep is a different
	 * *number* of folds and not only a different set of them.
	 */
	const deep = (entries: number) =>
		Object.fromEntries(
			Array.from({ length: entries }, (_, index) => [
				`key-${index}`,
				{ level2: { a: { leaf: index }, b: { leaf: index } } }
			])
		);

	it('folds everything past level two once it is over the line count (#4)', async () => {
		const value = deep(120);
		expect(JSON.stringify(value, null, 2).split('\n').length).toBeGreaterThan(FOLD_OVER_LINES);
		render(JsonView, { value, label: 'Metadata' });
		await painted();

		// The whole document is still there; what changed is what is drawn.
		expect(text('Metadata')).toBe(JSON.stringify(value, null, 2));
		// One fold per `level2` object — level 0 is the root, level 1 is each
		// value under it, and level 2 is where the tree used to close.
		expect(folds('Metadata')).toBe(120);
	});

	it('puts the document on screen before it folds it (#4)', async () => {
		render(JsonView, { value: deep(120), label: 'Metadata' });

		// Finding the folds means parsing the whole payload, and a megabyte of
		// it before the first frame is a panel that opens blank and stays that
		// way for as long as the parse takes. The document comes first.
		expect(text('Metadata').length).toBeGreaterThan(0);
		expect(folds('Metadata')).toBe(0);

		await painted();
		expect(folds('Metadata')).toBe(120);
	});

	it('opens a short one flat', async () => {
		const value = deep(3);
		expect(JSON.stringify(value, null, 2).split('\n').length).toBeLessThan(FOLD_OVER_LINES);
		render(JsonView, { value, label: 'Metadata' });
		await painted();

		expect(folds('Metadata')).toBe(0);
	});

	it('lets a caller override the rule in both directions', async () => {
		render(JsonView, { value: deep(3), label: 'Short', folded: true });
		render(JsonView, { value: deep(120), label: 'Long', folded: false });
		await painted();

		expect(folds('Short')).toBe(3);
		expect(folds('Long')).toBe(0);
	});
});

describe('the editor', () => {
	it('says where a document stopped being JSON (#8)', () => {
		expect(problem('{"a": 1}')).toBeNull();

		// The position is what makes the message a destination rather than a
		// verdict, and it is the engine's own: this one names character 5.
		expect(problem('{"a" 1}')?.at).toBe(5);

		// Both phrasings, because the engines disagree and neither promises
		// to keep the one it has.
		expect(errorPosition('Unexpected token in JSON at position 12', 'x'.repeat(40))).toBe(12);
		expect(errorPosition('unexpected character at line 3 column 4', 'ab\ncde\nfghi\n')).toBe(10);

		// And where a message carries no number at all — which is what V8
		// answers for the document below — the end of it is honest.
		const broken = '{"a": }';
		expect(/position \d|line \d/.test(problem(broken)?.message ?? '')).toBe(false);
		expect(problem(broken)?.at).toBe(broken.length);
		// Never past the document, whatever the engine claimed.
		expect(errorPosition('at position 900', broken)).toBe(broken.length);
	});

	it('reports `valid` false while the text does not parse, and true after the fix', async () => {
		const props = $state({ text: '{"a": }', label: 'Item', valid: true });
		render(JsonEditor, props);
		flushSync();

		// Answered on mount, before the linter has had a pause to run in: a
		// consumer that reads `valid` to gate a save is not told "yes" for the
		// first 300 ms of a document that never parsed.
		expect(props.valid).toBe(false);
		// And says so in place, at the offending position rather than as a
		// verdict on the whole document.
		expect(await marks('Item')).toEqual([{ from: 7, to: 7 }]);

		props.text = '{"a": 1}';
		flushSync();

		expect(text('Item')).toBe('{"a": 1}');
		expect(await marks('Item')).toEqual([]);
		expect(props.valid).toBe(true);

		// And back: every change is answered, not only the first one.
		props.text = '{"a": ';
		flushSync();
		await linted('Item');
		expect(props.valid).toBe(false);
	});

	it('parses the document once a pause, not once a keystroke', async () => {
		const props = $state({ text: '{"a": 1}', label: 'Item', valid: true });
		render(JsonEditor, props);
		await linted('Item');

		const parse = vi.spyOn(JSON, 'parse');
		for (const typed of ['{"a": 1', '{"a": 12', '{"a": 123', '{"a": 1234}']) {
			props.text = typed;
			flushSync();
		}

		// Four changes with no pause between them: the mark on screen and the
		// flag the consumer reads are the same parse, and it has not run yet.
		expect(parse).not.toHaveBeenCalled();

		await linted('Item');
		expect(parse.mock.calls.length).toBe(1);
		expect(props.valid).toBe(true);
		parse.mockRestore();
	});

	it('is turned off and on again in place, not rebuilt', async () => {
		const props = $state({ text: '{"a": 1}', label: 'Item', disabled: false });
		render(JsonEditor, props);
		const before = editor('Item');
		before.dispatch({ changes: { from: 8, insert: ' ' }, selection: { anchor: 3 } });

		props.disabled = true;
		flushSync();
		expect(editor('Item').state.readOnly).toBe(true);

		props.disabled = false;
		flushSync();
		const after = editor('Item');

		// The same instance: a consumer that disables the editor while it
		// saves is not asking for a different one, and a rebuilt editor loses
		// the cursor, the undo history, an open search panel and every fold
		// the reader opened by hand.
		expect(after).toBe(before);
		expect(after.state.readOnly).toBe(false);
		expect(after.state.selection.main.head).toBe(3);
		expect(undo(after)).toBe(true);
		expect(text('Item')).toBe('{"a": 1}');
	});

	it('answers `valid` while it is disabled', async () => {
		const props = $state({ text: '{"a": 1}', label: 'Item', valid: true, disabled: true });
		render(JsonEditor, props);
		flushSync();
		expect(props.valid).toBe(true);

		// A document does not stop being invalid because the consumer is
		// saving, and `valid` is this component's one output (#8).
		props.text = '{"a": ';
		flushSync();
		await linted('Item');
		expect(props.valid).toBe(false);
	});

	it('formats a valid document and leaves an invalid one alone', async () => {
		expect(format('{"a":1}')).toBe('{\n  "a": 1\n}');
		expect(format('{"a": }')).toBeNull();

		const props = $state({ text: '{"a":1,"b":[2]}', label: 'Item' });
		render(JsonEditor, props);
		const press = () => userEvent.setup().click(screen.getByRole('button', { name: 'Format' }));

		await press();
		expect(props.text).toBe('{\n  "a": 1,\n  "b": [\n    2\n  ]\n}');

		props.text = '{"a": }';
		flushSync();
		await press();
		expect(props.text).toBe('{"a": }');
	});

	it('does not swallow Tab, so a keyboard can leave it (#7)', () => {
		render(JsonEditor, { text: '{}', label: 'Item' });

		const tab = new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true });
		screen.getByLabelText('Item').dispatchEvent(tab);

		// Nothing claimed it, so the browser moves focus on: a contenteditable
		// that eats Tab is a keyboard trap (WCAG 2.1.2).
		expect(tab.defaultPrevented).toBe(false);
	});
});

describe('copying', () => {
	it('writes the whole document, not the part that is drawn', async () => {
		const user = userEvent.setup();
		const value = { role: 'user', content: 'x'.repeat(4000) };
		render(JsonView, { value, label: 'Input' });

		await user.click(screen.getByRole('button', { name: /copy the whole input/i }));

		expect(await navigator.clipboard.readText()).toBe(JSON.stringify(value, null, 2));
	});
});
