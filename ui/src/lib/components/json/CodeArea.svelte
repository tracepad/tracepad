<script lang="ts">
	import { EditorState } from '@codemirror/state';
	import { EditorView } from '@codemirror/view';
	import { untrack, type Snippet } from 'svelte';
	import type { Attachment } from 'svelte/attachments';
	import CopyButton from '../CopyButton.svelte';
	import { FOLD_OVER_LINES, blank, extensions, foldDeep, lintAnswer, problem, relock } from './setup';

	// The surface itself: a CodeMirror instance, a toolbar over it, and the
	// caller's banner over that (spec 015, Component contract). It is internal
	// — `JsonView` and `JsonEditor` are what the rest of the interface uses —
	// and it knows nothing about payloads, budgets or truncation markers. The
	// banner is a snippet for exactly that reason: what a truncated payload
	// says is the payload wrapper's business, not the editor's (#3).

	let {
		text = $bindable(''),
		label,
		readonly = false,
		plain = false,
		folded,
		valid = $bindable(true),
		disabled = false,
		optional = false,
		whole = true,
		banner,
		actions
	}: {
		/** The document. Bound in the editor, read in the viewer. */
		text?: string;
		/** The accessible name of the editing region. */
		label: string;
		readonly?: boolean;
		/** Not JSON at all: no language, no diagnostics (#12). */
		plain?: boolean;
		/** Overrides the length rule of #4 in both directions. */
		folded?: boolean;
		/**
		 * Whether the document parses. Only an editor reports it (#8), and it
		 * is the linter's answer, so it settles a pause after the last
		 * keystroke rather than during it.
		 */
		valid?: boolean;
		disabled?: boolean;
		/**
		 * Whether an empty document is a state this field may be in (spec 016
		 * #22). It is then neither marked nor reported invalid: "" is not
		 * broken JSON, it is a field the author left out.
		 */
		optional?: boolean;
		/**
		 * Whether what is on screen is the whole of what the caller has. A
		 * truncated payload's preview is not, and Copy is not offered for it:
		 * a button promising the whole payload that put a prefix on the
		 * clipboard would be worse than no button (#3).
		 */
		whole?: boolean;
		banner?: Snippet;
		actions?: Snippet;
	} = $props();

	let view = $state.raw<EditorView | null>(null);

	// What this instance is, fixed for its lifetime, and whether it will take
	// a keystroke right now, which is not: `disabled` is reconfigured into the
	// live editor below rather than being a reason to build another one.
	const editing = $derived(!readonly && !plain);
	const locked = $derived(readonly || disabled);

	/**
	 * Builds the instance. The document is read untracked so that a keystroke
	 * does not tear the editor down and put it back — the arguments are what
	 * this attachment re-runs on, and they are the things that really are a
	 * different editor: a different language, a different name for the region,
	 * a viewer rather than an editor.
	 */
	function mount(label: string, editing: boolean, plain: boolean, optional: boolean): Attachment {
		return (node) => {
			const created = new EditorView({
				parent: node,
				state: EditorState.create({
					doc: untrack(() => text),
					extensions: [
						extensions({ label, editing, isLocked: untrack(() => locked), plain, optional }),
						EditorView.updateListener.of((update) => {
							if (update.docChanged) text = update.state.doc.toString();
							const answer = lintAnswer(update);
							if (answer !== null) valid = answer;
						})
					]
				})
			});
			// The one parse this component does itself, so that a consumer
			// reading `valid` on mount is not told "yes" for the 300 ms it
			// takes the linter to have an opinion.
			if (editing) {
				const doc = created.state.doc.toString();
				valid = (optional && blank(doc)) || problem(doc) === null;
			}

			// Folded after the browser has painted the document, not before:
			// finding the folds means parsing the whole payload, and a
			// megabyte of it is a panel that stays blank for as long as that
			// takes (#4). A frame later the reader is already reading.
			const wanted = untrack(() => folded);
			const frame = requestAnimationFrame(() => {
				if (wanted ?? created.state.doc.lines > FOLD_OVER_LINES) foldDeep(created);
			});

			view = created;
			return () => {
				cancelAnimationFrame(frame);
				view = null;
				created.destroy();
			};
		};
	}

	// `disabled` in place: the same editor, with the cursor, the history, the
	// search panel and every fold the reader opened by hand still in it.
	$effect(() => {
		const editor = view;
		const now = locked;
		if (editor) relock(editor, now, untrack(() => editing));
	});

	// The document a consumer replaced under the editor — spec 016's item
	// swapping for another one, and nothing else. Comparing before dispatching
	// is what keeps this from fighting the update listener above, and what
	// keeps a keystroke from resetting the cursor to the end of the line.
	$effect(() => {
		const wanted = text;
		const editor = view;
		if (!editor) return;
		untrack(() => {
			const current = editor.state.doc.toString();
			if (current === wanted) return;
			editor.dispatch({ changes: { from: 0, to: current.length, insert: wanted } });
		});
	});
</script>

<div class="flex min-w-0 flex-col gap-1.5">
	{#if banner}{@render banner()}{/if}

	{#if actions || whole}
		<div class="flex items-center justify-end gap-1">
			{#if actions}{@render actions()}{/if}
			{#if whole}
				<CopyButton text={() => text} label="Copy the whole {label}" />
			{/if}
		</div>
	{/if}

	<div
		class={['min-w-0', disabled && 'opacity-60']}
		{@attach mount(label, editing, plain, optional)}
	></div>
</div>
