<script lang="ts">
	import { EditorState } from '@codemirror/state';
	import { EditorView } from '@codemirror/view';
	import { untrack, type Snippet } from 'svelte';
	import type { Attachment } from 'svelte/attachments';
	import CopyButton from '../CopyButton.svelte';
	import { FOLD_OVER_LINES, extensions, foldDeep, problem } from './setup';

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
		/** Whether the document parses. Only an editor reports it (#8). */
		valid?: boolean;
		disabled?: boolean;
		banner?: Snippet;
		actions?: Snippet;
	} = $props();

	let view = $state.raw<EditorView | null>(null);

	const locked = $derived(readonly || disabled);
	const lints = $derived(!locked && !plain);

	/**
	 * Builds the instance. The document is read untracked so that a keystroke
	 * does not tear the editor down and put it back — the arguments are what
	 * this attachment re-runs on, and they are the things that really are a
	 * different editor: a different mode, a different language, a different
	 * name for the region.
	 */
	function mount(label: string, readOnly: boolean, plain: boolean): Attachment {
		return (node) => {
			const created = new EditorView({
				parent: node,
				state: EditorState.create({
					doc: untrack(() => text),
					extensions: [
						extensions({ label, readOnly, plain }),
						EditorView.updateListener.of((update) => {
							if (!update.docChanged) return;
							const next = update.state.doc.toString();
							text = next;
							if (lints) valid = problem(next) === null;
						})
					]
				})
			});
			if (untrack(() => folded) ?? created.state.doc.lines > FOLD_OVER_LINES) foldDeep(created);
			if (untrack(() => lints)) valid = problem(created.state.doc.toString()) === null;
			view = created;
			return () => {
				view = null;
				created.destroy();
			};
		};
	}

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

	<div class="flex items-center justify-end gap-1">
		{#if actions}{@render actions()}{/if}
		<CopyButton text={() => text} label="Copy the whole {label}" />
	</div>

	<div class={['min-w-0', disabled && 'opacity-60']} {@attach mount(label, locked, plain)}></div>
</div>
