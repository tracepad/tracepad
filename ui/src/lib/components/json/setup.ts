import { defaultKeymap, history, historyKeymap } from '@codemirror/commands';
import { json } from '@codemirror/lang-json';
import {
	HighlightStyle,
	ensureSyntaxTree,
	foldEffect,
	foldGutter,
	foldInside,
	foldKeymap,
	syntaxHighlighting,
	syntaxTree
} from '@codemirror/language';
import { diagnosticCount, linter, setDiagnosticsEffect } from '@codemirror/lint';
import { search, searchKeymap } from '@codemirror/search';
import { Compartment, EditorState, type Extension } from '@codemirror/state';
import { EditorView, highlightActiveLine, keymap, type ViewUpdate } from '@codemirror/view';
import { tags } from '@lezer/highlight';

// The one CodeMirror setup the whole interface reads and writes JSON through
// (spec 015 #1). Everything that is not a DOM node lives here so that it can
// be read — and tested — without mounting anything: what a value looks like as
// a document, where a document stops being JSON, which nodes a long one opens
// folded, and the extension list itself.
//
// There is no `basicSetup` (#1): it carries autocompletion, bracket matching
// and a keymap this surface does not want, and a bundle line the PR could not
// explain. Every extension below is here because a decision asked for it.

/** A document longer than this opens folded (#4). */
export const FOLD_OVER_LINES = 400;

/**
 * How much of a folded document stays open: the two levels the hand-written
 * tree opened, carried over where they still earn their place (#4). The root
 * value is level 0, so the containers nested two deep are the ones that close.
 */
const OPEN_DEPTH = 2;

/**
 * How long the parser may spend finding those folds. A payload after the whole
 * of it has been loaded can be megabytes, and the alternative to a bound is a
 * mount that blocks for as long as the document is big; what parsed in time is
 * folded and the rest opens flat, which is the old tree's behaviour anyway.
 *
 * It is a ceiling for the pathological document rather than a price the
 * ordinary one pays (#16): a parse that finishes inside the budget costs the
 * same whatever the budget is, so the number only decides where folding gives
 * up. Measured at 3.9 MB, folding whole costs ~330 ms of blocking and needs
 * between 250 and 300 ms of this to get there; 400 leaves that headroom for a
 * slower machine and still bounds a screen of four payloads at 1.6 s rather
 * than the 4 s a second each would allow.
 */
const PARSE_BUDGET_MS = 400;

/**
 * How a value becomes a document. A string is its own text — the API returns a
 * bare string for a plain-span prompt, and quoting it would put an escape on
 * every newline of the thing the reader came to read (#12). Anything else is
 * JSON at two-space indentation (#2).
 */
export function asDocument(value: unknown): { text: string; plain: boolean } {
	if (typeof value === 'string') return { text: value, plain: true };
	return { text: JSON.stringify(value, null, 2) ?? String(value), plain: false };
}

/** Where a document stops being JSON, and what the engine called it (#8). */
export type Problem = { message: string; at: number };

export function problem(text: string): Problem | null {
	try {
		JSON.parse(text);
		return null;
	} catch (cause) {
		const message = cause instanceof Error ? cause.message : 'This is not JSON.';
		return { message, at: errorPosition(message, text) };
	}
}

/**
 * The offset the engine blamed. `JSON.parse` is the only parser here, so its
 * message is the only position source, and the engines phrase it differently:
 * V8 counts characters from the start, Firefox counts a line and a column, and
 * both of them drop the number entirely for some inputs. Where there is none,
 * the end of the document is honest — it is where an unterminated object
 * really did run out, and it is never a position that is not in the document.
 */
export function errorPosition(message: string, text: string): number {
	const byOffset = /at position (\d+)/.exec(message);
	if (byOffset) return Math.min(Number(byOffset[1]), text.length);

	const byLine = /line (\d+) column (\d+)/.exec(message);
	if (byLine) {
		const lines = text.split('\n');
		const line = Math.min(Number(byLine[1]), lines.length);
		let at = 0;
		for (let before = 0; before < line - 1; before++) at += lines[before].length + 1;
		return Math.min(at + Number(byLine[2]) - 1, text.length);
	}

	return text.length;
}

/**
 * Pretty-prints a document that parses and answers `null` for one that does
 * not: Format is a transform, never a repair (#8). Sorting keys or quoting
 * what looks like a key would be the component deciding what the document
 * means, which is the author's business.
 */
export function format(text: string): string | null {
	try {
		return JSON.stringify(JSON.parse(text), null, 2);
	} catch {
		return null;
	}
}

type Node = ReturnType<typeof syntaxTree>['topNode'];

/**
 * The ranges a long document opens folded: every object and array nested
 * `OPEN_DEPTH` containers deep or deeper (#4). A folded container's own
 * contents are not walked — folding what is already inside a fold would only
 * cost a transaction nobody can see the effect of.
 */
export function deepFolds(state: EditorState): { from: number; to: number }[] {
	const tree = ensureSyntaxTree(state, state.doc.length, PARSE_BUDGET_MS) ?? syntaxTree(state);
	const ranges: { from: number; to: number }[] = [];

	const walk = (parent: Node, depth: number) => {
		for (let child = parent.firstChild; child; child = child.nextSibling) {
			const container = child.name === 'Object' || child.name === 'Array';
			if (container && depth >= OPEN_DEPTH) {
				const range = foldInside(child);
				if (range) ranges.push(range);
				continue;
			}
			walk(child, container ? depth + 1 : depth);
		}
	};
	walk(tree.topNode, 0);
	return ranges;
}

/** Folds a document that asked for it, in one transaction. */
export function foldDeep(view: EditorView) {
	const ranges = deepFolds(view.state);
	if (ranges.length > 0) view.dispatch({ effects: ranges.map((range) => foldEffect.of(range)) });
}

/**
 * The syntax colours, from the token file and nowhere else (#6). Keys and
 * punctuation are what a reader distinguishes a JSON document by at a glance,
 * which is why they are colours of their own rather than `fg` and `muted`;
 * `punctuation` is the parent tag of the separators and every bracket, so one
 * rule covers `,`, `:`, `[]` and `{}`.
 */
const highlight = HighlightStyle.define([
	{ tag: tags.propertyName, color: 'var(--color-code-key)' },
	{ tag: tags.string, color: 'var(--color-code-string)' },
	{ tag: [tags.number, tags.bool, tags.null], color: 'var(--color-code-number)' },
	{ tag: tags.punctuation, color: 'var(--color-code-punct)' }
]);

/**
 * The editor's chrome, in the same tokens as the rest of the interface (#6).
 * A shipped CodeMirror theme brings its own palette and its own idea of dark,
 * and would be the one part of the screen the toggle does not reach.
 *
 * `dark` is deliberately not set: `light-dark()` resolves per theme inside
 * every value below, so there is nothing for a second switch to decide.
 */
const surface = EditorView.theme({
	'&': {
		color: 'var(--color-fg)',
		backgroundColor: 'var(--color-surface)',
		border: '1px solid var(--color-border)',
		borderRadius: 'var(--radius-md)',
		fontSize: 'var(--text-xs)',
		// Grows with the document, then scrolls inside itself rather than
		// pushing the page around it (Component contract).
		maxHeight: '60vh'
	},
	'&.cm-focused': { outline: '2px solid var(--color-accent)', outlineOffset: '1px' },
	'.cm-scroller': {
		fontFamily: 'var(--font-mono)',
		lineHeight: 'var(--text-xs--line-height)',
		overflowX: 'hidden'
	},
	'.cm-content': { padding: '0.25rem 0', caretColor: 'var(--color-fg)' },
	'.cm-line': { padding: '0 0.5rem' },
	'.cm-cursor, .cm-dropCursor': { borderLeftColor: 'var(--color-fg)' },
	'&.cm-focused .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection': {
		backgroundColor: 'var(--color-accent-soft)'
	},
	'.cm-activeLine': { backgroundColor: 'var(--color-raised)' },
	'.cm-gutters': {
		backgroundColor: 'var(--color-surface)',
		color: 'var(--color-subtle)',
		border: 'none'
	},
	'.cm-foldGutter .cm-gutterElement': { padding: '0 0.125rem 0 0.25rem', cursor: 'pointer' },
	'.cm-foldPlaceholder': {
		backgroundColor: 'var(--color-raised)',
		border: '1px solid var(--color-border)',
		borderRadius: 'var(--radius-sm)',
		color: 'var(--color-muted)',
		margin: '0 0.125rem',
		padding: '0 0.25rem'
	},
	'.cm-panels': { backgroundColor: 'var(--color-raised)', color: 'var(--color-fg)' },
	'.cm-panels.cm-panels-top': { borderBottom: '1px solid var(--color-border)' },
	'.cm-panel.cm-search': {
		fontFamily: 'var(--font-sans)',
		fontSize: 'var(--text-sm)',
		padding: '0.375rem 0.5rem'
	},
	// The same border, ground, radius and size the app's form fields wear
	// (`FilterBar`'s `fieldClass`); CodeMirror builds these inputs itself, so
	// the classes cannot be put on them and the tokens are spent here instead.
	'.cm-panel.cm-search input:not([type="checkbox"]), .cm-panel.cm-search button': {
		backgroundColor: 'var(--color-canvas)',
		// CodeMirror paints its buttons with a pale gradient of its own, which
		// in dark mode is a light plate under light text.
		backgroundImage: 'none',
		border: '1px solid var(--color-border)',
		borderRadius: 'var(--radius-md)',
		color: 'var(--color-fg)',
		fontSize: 'var(--text-sm)',
		padding: '0.125rem 0.375rem'
	},
	'.cm-panel.cm-search button': { cursor: 'pointer' },
	'.cm-panel.cm-search button:hover': { backgroundColor: 'var(--color-raised)' },
	'.cm-panel.cm-search label': { color: 'var(--color-muted)' },
	'.cm-searchMatch': { backgroundColor: 'var(--color-accent-soft)' },
	'.cm-searchMatch.cm-searchMatch-selected': {
		backgroundColor: 'var(--color-accent)',
		color: 'var(--color-on-accent)'
	},
	'.cm-tooltip': {
		backgroundColor: 'var(--color-raised)',
		border: '1px solid var(--color-border)',
		borderRadius: 'var(--radius-md)',
		color: 'var(--color-fg)'
	},
	'.cm-diagnostic': { borderLeftColor: 'var(--color-danger)', padding: '0.25rem 0.5rem' }
});

/**
 * The compartment `disabled` is reconfigured through. A consumer that turns
 * an editor off while it saves is not asking for a different editor, and
 * rebuilding one would throw away the cursor, the undo history, an open
 * search panel and every fold the reader had opened by hand.
 */
const locking = new Compartment();

/** What being locked decides: the facet, and the affordance that offers typing. */
function locked(isLocked: boolean, editing: boolean): Extension {
	return [
		EditorState.readOnly.of(isLocked),
		...(editing && !isLocked ? [highlightActiveLine()] : [])
	];
}

/** Turns an instance off and on again in place, keeping everything else. */
export function relock(view: EditorView, isLocked: boolean, editing: boolean) {
	if (view.state.readOnly === isLocked) return;
	view.dispatch({ effects: locking.reconfigure(locked(isLocked, editing)) });
}

/**
 * The linter's answer, when a transaction carried one. It is the only parse
 * of the document there is: `valid` is what the diagnostic already knows, so
 * an author gets one parse per pause rather than one per keystroke, and the
 * mark on screen and the flag the consumer reads can never disagree (#8).
 */
export function lintAnswer(update: ViewUpdate): boolean | null {
	const answered = update.transactions.some((tr) =>
		tr.effects.some((effect) => effect.is(setDiagnosticsEffect))
	);
	return answered ? diagnosticCount(update.state) === 0 : null;
}

/**
 * Everything one instance is made of. The mode is the `readOnly` facet and
 * nothing else (#2): the viewer stays a focusable, selectable, searchable
 * document, because a payload nobody can put the cursor in is a payload
 * `Cmd-F` cannot reach (#5).
 *
 * `editing` is what this instance *is* — an editor rather than a viewer, over
 * a document that is JSON at all — and is fixed for its lifetime; `isLocked`
 * is whether it will take a keystroke right now, and is not.
 */
export function extensions(options: {
	label: string;
	editing: boolean;
	isLocked: boolean;
	plain: boolean;
}): Extension[] {
	const { label, editing, isLocked, plain } = options;
	return [
		EditorView.lineWrapping,
		EditorView.contentAttributes.of({ 'aria-label': label }),
		locking.of(locked(isLocked, editing)),
		foldGutter(),
		search({ top: true }),
		history(),
		// `indentWithTab` is deliberately absent (#7): Tab is how a keyboard
		// leaves this control, and a `contenteditable` that swallows it is a
		// trap. Escape is bound by `searchKeymap` to closing the search panel
		// and falls through to the page — the peek panel's close — when there
		// is no panel open.
		keymap.of([...defaultKeymap, ...historyKeymap, ...searchKeymap, ...foldKeymap]),
		syntaxHighlighting(highlight),
		surface,
		...(plain ? [] : [json()]),
		// The linter stays installed while the editor is off: a document does
		// not stop being invalid because the consumer is saving, and `valid`
		// is read from what it finds.
		...(editing ? [diagnostics] : [])
	];
}

/**
 * The parse error, in place (#8). An author told only "invalid JSON" goes
 * hunting; the position is what turns the message into a destination.
 */
const diagnostics = linter(
	(view) => {
		const found = problem(view.state.doc.toString());
		if (!found) return [];
		const at = Math.min(found.at, view.state.doc.length);
		return [
			{
				from: at,
				to: Math.min(at + 1, view.state.doc.length),
				severity: 'error' as const,
				message: found.message
			}
		];
	},
	{ delay: 300 }
);
