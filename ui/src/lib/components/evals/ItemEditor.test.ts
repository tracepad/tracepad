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
vi.mock('$app/state', () => ({ page: { url: new URL('http://tracepad.test/datasets/items/new') } }));
vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		listDatasets: async () => ({ datasets: [{ name: 'golden' }] }),
		getItem: vi.fn(),
		getObservationIO: vi.fn(),
		putItem: (...args: unknown[]) => putItem(...(args as [])),
	}
}));

/** The instance behind a named region, the way CodeMirror finds one itself. */
function editor(label: string): EditorView {
	const found = EditorView.findFromDOM(screen.getByLabelText(label) as HTMLElement);
	if (!found) throw new Error(`no editor under ${label}`);
	return found;
}

/** Types a whole document, then waits for the linter that answers `valid`. */
async function write(label: string, text: string) {
	const view = editor(label);
	view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
	forceLinting(view);
	await new Promise((wake) => setTimeout(wake, 0));
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

	it('says so when the write changed nothing', async () => {
		const user = userEvent.setup();
		putItem.mockResolvedValueOnce({ ids: ['b'.repeat(32)], version: 7, changed: 0 });
		render(ItemEditor, { dataset: 'golden' } as never);
		await write('Input', '{"q": 1}');

		await user.click(save());

		expect(await screen.findByText(/^Unchanged/)).toHaveTextContent('version 7');
	});
});
