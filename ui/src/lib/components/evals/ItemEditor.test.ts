import { forceLinting } from '@codemirror/lint';
import { EditorView } from '@codemirror/view';
import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import ItemEditor from './ItemEditor.svelte';

// The item editor's gate and its answer (spec 016 #5). Save is shut until the
// input is a document and nothing else is a broken one — an empty *Expected
// output* is a field left out, not a syntax error (#22) — and what a save
// reports is the store's own sentence: a version, or "unchanged".

const putItem = vi.fn(async () => ({ ids: ['a'.repeat(32)], version: 2, changed: 1 }));

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({
	page: {
		url: new URL(`http://tracepad.test/p/${'a'.repeat(32)}/datasets/items/new`),
		params: { project: 'a'.repeat(32) }
	}
}));
vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		listDatasets: async () => ({ datasets: [{ name: 'golden' }], next_cursor: null }),
		getItem: vi.fn(),
		getObservationIO: async () => ({
			input: { question: 'which plan?' },
			output: { answer: 'the raw one the model gave' }
		}),
		putItem: (...args: unknown[]) => putItem(...(args as [])),
	}
}));

/** The instance behind a named region, the way CodeMirror finds one itself. */
function editor(label: string): EditorView {
	const found = EditorView.findFromDOM(screen.getByLabelText(label) as HTMLElement);
	if (!found) throw new Error(`no editor under ${label}`);
	return found;
}

/**
 * The pause the linter answers in, which is what `valid` settles on (spec 015
 * #15) — including for a pane the editor filled in rather than the author.
 */
async function lint(label: string) {
	forceLinting(editor(label));
	await new Promise((wake) => setTimeout(wake, 0));
}

/** Types a whole document, then waits for the linter that answers `valid`. */
async function write(label: string, text: string) {
	const view = editor(label);
	view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
	await lint(label);
}

const save = () => screen.getByRole('button', { name: 'Save' });

describe('the save gate', () => {
	it('opens on an input that parses and an empty optional pane', async () => {
		render(ItemEditor, { dataset: 'golden' } as never);
		expect(save()).toBeDisabled();

		await write('Input', '{"q": "refund?"}');

		expect(save()).toBeEnabled();
	});

	it('stays shut while another pane is a broken document', async () => {
		render(ItemEditor, { dataset: 'golden' } as never);
		await write('Input', '{"q": 1}');
		await write('Expected output', '{"a": ');

		expect(save()).toBeDisabled();
	});

	it('stays shut until a dataset is chosen', async () => {
		render(ItemEditor, { dataset: '' } as never);
		await write('Input', '{"q": 1}');

		expect(save()).toBeDisabled();
	});

	// The items endpoint brings a dataset into being on the first write to its
	// name, so a `?dataset=` naming nothing — a stale link, a typo — would
	// quietly create a misspelled dataset (found in review of this PR).
	it('stays shut for a dataset the project does not have', async () => {
		render(ItemEditor, { dataset: 'ghost' } as never);
		await write('Input', '{"q": 1}');

		expect(await screen.findByText(/no dataset called/)).toBeInTheDocument();
		expect(save()).toBeDisabled();
	});
});

describe('what a save sends and says', () => {
	it('posts the parsed panes and reports the version', async () => {
		const user = userEvent.setup();
		render(ItemEditor, { dataset: 'golden' } as never);
		await write('Input', '{"q": 1}');
		await write('Metadata', '{"note": "seen in prod"}');

		await user.click(save());

		expect(putItem).toHaveBeenCalledWith('golden', {
			input: { q: 1 },
			metadata: { note: 'seen in prod' }
		});
		expect(await screen.findByText('Saved as version 2.')).toBeInTheDocument();
	});

	// The panes are the observation's, whole, and the source pair travels with
	// them (spec 016 #8). That the panes survive *picking a dataset* is the
	// same promise one URL change later, and is asserted end to end, where the
	// route really does rewrite `?dataset=` under a mounted editor.
	it('posts the observation it was opened on, with where it came from', async () => {
		const user = userEvent.setup();
		render(ItemEditor, { dataset: 'golden', trace: 'e0a1', obs: 'e2e3' } as never);
		// Only once the payloads are in the panes: they take no keystroke before
		// that, because the prefill assigns all three when it lands.
		await screen.findByText(/which plan\?/);
		await write('Expected output', '{"answer": "the one it should have given"}');
		await lint('Input');

		await user.click(save());

		expect(putItem).toHaveBeenCalledWith('golden', {
			input: { question: 'which plan?' },
			expected_output: { answer: 'the one it should have given' },
			source_trace_id: 'e0a1',
			source_observation_id: 'e2e3'
		});
	});

	it('says so when the write changed nothing', async () => {
		const user = userEvent.setup();
		putItem.mockResolvedValueOnce({ ids: ['b'.repeat(32)], version: 7, changed: 0 });
		render(ItemEditor, { dataset: 'golden' } as never);
		await write('Input', '{"q": 1}');

		await user.click(save());

		expect(await screen.findByText(/^Unchanged/)).toHaveTextContent('version 7');
	});
});
