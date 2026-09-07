import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ScoreConfig } from '$lib/api/client.svelte';
import NewQueueDialog from './NewQueueDialog.svelte';

// The New-queue form's gate (spec 024 #10): a name the server would take, and
// at least one score — a queue is the scores it asks for.

// Hoisted so the module's own `ApiError` is the one the dialog checks
// `instanceof` against: a 404 from `getQueue` is how it learns the name is
// free, and a stand-in of a different class would read as a real failure.
const { ApiError, putQueue, getQueue } = vi.hoisted(() => {
	class ApiError extends Error {
		constructor(
			readonly status: number,
			message: string
		) {
			super(message);
		}
	}
	return {
		ApiError,
		putQueue: vi.fn(async () => ({ name: 'weekly' })),
		getQueue: vi.fn(async () => {
			throw new ApiError(404, 'queue not found');
		})
	};
});

vi.mock('$lib/api/client.svelte', () => ({ ApiError, api: { putQueue, getQueue } }));

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));

const config = (name: string, type = 'numeric'): ScoreConfig => ({
	name,
	data_type: type as ScoreConfig['data_type'],
	direction: 'higher',
	min: null,
	max: null,
	categories: null,
	description: null,
	created_at: '2026-09-07T10:00:00Z',
	updated_at: '2026-09-07T10:00:00Z'
});

function dialog(configs: ScoreConfig[] = [config('accuracy'), config('tone', 'categorical')]) {
	render(NewQueueDialog, {
		props: { open: true, configs, onclose: vi.fn() }
	} as never);
	return userEvent.setup({ delay: null });
}

const create = () => screen.getByRole('button', { name: 'Create' });

beforeEach(() => {
	putQueue.mockClear();
	document.body.style.pointerEvents = '';
});

describe('the gate', () => {
	it('refuses a queue with no score picked, however good the name', async () => {
		const user = dialog();
		await user.type(screen.getByLabelText('Name'), 'weekly-review');

		expect(create()).toBeDisabled();
		expect(screen.getByText(/at least one score/)).toBeVisible();
	});

	it('refuses a name the server would refuse, and says which rule', async () => {
		const user = dialog();
		await user.click(screen.getByRole('checkbox', { name: /accuracy/ }));
		await user.type(screen.getByLabelText('Name'), '-weekly');

		expect(create()).toBeDisabled();
		expect(screen.getByText(/starts with a letter or a digit/)).toBeVisible();
	});

	it('opens the gate on a name and a score, and posts them in the order picked', async () => {
		const user = dialog();
		await user.type(screen.getByLabelText('Name'), 'weekly-review');
		await user.click(screen.getByRole('checkbox', { name: /tone/ }));
		await user.click(screen.getByRole('checkbox', { name: /accuracy/ }));

		expect(create()).toBeEnabled();
		await user.click(create());

		await waitFor(() =>
			expect(putQueue).toHaveBeenCalledWith('weekly-review', {
				score_configs: ['tone', 'accuracy']
			})
		);
	});

	// A project that declared no configs cannot have a queue, because a queue
	// names configs and the server refuses a name that is not one.
	it('sends a project with no configs to declare one first', () => {
		dialog([]);

		expect(screen.getByRole('link', { name: 'Score configs' })).toHaveAttribute(
			'href',
			'/score-configs'
		);
		expect(create()).toBeDisabled();
	});
});
