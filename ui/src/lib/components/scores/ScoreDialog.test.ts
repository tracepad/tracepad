import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, type Score, type ScoreConfig } from '$lib/api/client.svelte';
import ScoreDialog from './ScoreDialog.svelte';

// The dialog's one rule (spec 022 #4): the control is the one the picked
// config dictates, and the free-name path — the way out for a project that
// declared no configs — is the only one that asks for a type. Edit is the same
// dialog re-posting the score's own id (#5).

const createScore = vi.fn(async () => ({ ids: ['a'.repeat(32)] }));

vi.mock('$lib/api/client.svelte', () => ({
	// The status first, as the real one takes it: the dialog shows the
	// server's own sentence, so the sentence has to survive the stand-in.
	ApiError: class extends Error {
		constructor(
			readonly status: number,
			message: string
		) {
			super(message);
		}
	},
	api: { createScore: (...args: unknown[]) => createScore(...(args as [])) }
}));

const config = (extra: Partial<ScoreConfig>): ScoreConfig => ({
	name: 'accuracy',
	data_type: 'numeric',
	direction: 'higher',
	min: 0,
	max: 1,
	categories: null,
	description: null,
	created_at: '2026-09-07T10:00:00Z',
	updated_at: '2026-09-07T10:00:00Z',
	...extra
});

const configs = [
	config({}),
	config({
		name: 'verdict',
		data_type: 'categorical',
		direction: null,
		min: null,
		max: null,
		categories: ['correct', 'wrong']
	})
];

const onsaved = vi.fn();

function dialog(score: Score | null = null, seeded: ScoreConfig[] = configs) {
	const props = {
		open: true,
		target: { trace_id: 'trace-1' },
		configs: seeded,
		score,
		onclose: vi.fn(),
		onsaved
	};
	// Under `props`, because `target` is also one of render's own options.
	render(ScoreDialog, { props } as never);
	// No delay between keystrokes: typing a value a character at a time over
	// a real second leaves the dialog open long enough for bits-ui's overlay
	// to race the content out from under the assertions.
	return userEvent.setup({ delay: null });
}

const save = () => screen.getByRole('button', { name: 'Save' });

beforeEach(() => {
	createScore.mockClear();
	onsaved.mockClear();
	document.body.style.pointerEvents = '';
});

describe('the control the config dictates', () => {
	it('gives a bounded number field to a numeric name', async () => {
		dialog();

		// `accuracy` is the first config, which is what a fresh dialog opens on.
		const value = screen.getByLabelText('Value');
		expect(value).toHaveAttribute('type', 'number');
		expect(value).toHaveAttribute('min', '0');
		expect(value).toHaveAttribute('max', '1');
		// No type radio: the config already says what the name means.
		expect(screen.queryByRole('radio')).toBeNull();
	});

	it('gives a categorical name its categories and nothing else', async () => {
		const user = dialog();
		await user.selectOptions(screen.getByLabelText('Name'), 'verdict');

		const value = screen.getByLabelText('Value');
		expect([...value.querySelectorAll('option')].map((one) => one.value)).toEqual([
			'',
			'correct',
			'wrong'
		]);
	});

	// A project that declared no configs has only the free path, and the
	// dialog opens on it. That the form then survives the configs *landing*
	// is not testable here — `@testing-library/svelte` keeps every prop in
	// one `$state.raw` object, so a rerender invalidates every prop read and
	// not only the one it changed — so that regression is held in the e2e
	// suite, where the two requests really are sequential.
	it('opens on the free path when the project declared nothing', () => {
		dialog(null, []);

		expect(screen.getByLabelText('Score name')).toBeVisible();
		// `other…` is the whole of the select, and its value is the empty name.
		expect(screen.getByLabelText('Name')).toHaveValue('');
		expect(screen.getAllByRole('option')).toHaveLength(1);
	});

	it('asks the free-name path for a type before it asks for a value', async () => {
		const user = dialog();
		// The dialog opens on the first declared name; *other…* is the way
		// out for a name the project never declared.
		await user.selectOptions(screen.getByLabelText('Name'), 'other…');
		await user.type(screen.getByLabelText('Score name'), 'vibes');

		// Nothing to fill in yet: the type is what decides the control.
		expect(screen.queryByLabelText('Value')).toBeNull();
		expect(save()).toBeDisabled();
		expect(screen.getByText(/kind of value/)).toBeVisible();

		await user.click(screen.getByRole('radio', { name: /boolean/ }));
		await user.click(screen.getByRole('button', { name: 'yes' }));

		expect(save()).toBeEnabled();
	});
});

describe('what Save posts', () => {
	it('stamps a new score as written here, and states the type', async () => {
		const user = dialog();
		await user.selectOptions(screen.getByLabelText('Name'), 'verdict');
		await user.selectOptions(screen.getByLabelText('Value'), 'correct');
		await user.click(save());

		expect(createScore).toHaveBeenCalledWith({
			trace_id: 'trace-1',
			name: 'verdict',
			data_type: 'categorical',
			string_value: 'correct',
			comment: '',
			metadata: { source: 'web' }
		});
		expect(onsaved).toHaveBeenCalled();
	});

	it('resends the id when it is editing, so the row is replaced', async () => {
		const editing: Score = {
			id: 'b'.repeat(32),
			name: 'accuracy',
			data_type: 'numeric',
			value: 0.4,
			metadata: { source: 'python-sdk' },
			timestamp: '2026-09-01T08:00:00Z',
			created_at: '2026-09-01T08:00:00Z'
		};
		const user = dialog(editing);

		const value = screen.getByLabelText('Value');
		expect(value).toHaveValue(0.4);
		await user.clear(value);
		await user.type(value, '0.9');
		await user.click(save());

		expect(createScore).toHaveBeenCalledWith(
			expect.objectContaining({
				id: 'b'.repeat(32),
				value: 0.9,
				// Neither the source nor the event time is an edit's to invent.
				metadata: { source: 'python-sdk' },
				timestamp: '2026-09-01T08:00:00Z'
			})
		);
	});

	it('shows the server refusal at the field it names', async () => {
		createScore.mockRejectedValueOnce(new ApiError(400, "value 1.5 is above the config's max 1"));
		const user = dialog();
		await user.selectOptions(screen.getByLabelText('Name'), 'accuracy');
		await user.type(screen.getByLabelText('Value'), '1.5');
		await user.click(save());

		const alert = await screen.findByRole('alert');
		expect(alert).toHaveTextContent(/above the config/);
		// Beside the value rather than down at Save: the refusal is about the
		// value, and the fix is in that field. It therefore stands above the
		// comment, which is the field after it.
		const after = alert.compareDocumentPosition(screen.getByLabelText(/Comment/));
		expect(after & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
	});
});
