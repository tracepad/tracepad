import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import AddToQueue from './AddToQueue.svelte';

// *Add to queue…* on the traces listing (spec 024 #13): it names how many the
// filters match *before* the call, and a filter matching more than the
// endpoint's cap is refused with the reason rather than truncated in silence.

const { listQueues, queueFromTraces, addQueueItems } = vi.hoisted(() => ({
	listQueues: vi.fn(async () => ({
		queues: [
			{ name: 'weekly', score_configs: ['accuracy'], counts: {} },
			{ name: 'errors', score_configs: ['accuracy'], counts: {} }
		]
	})),
	queueFromTraces: vi.fn(async () => ({ matched: 37, added: 37, existing: 0, capped: false })),
	addQueueItems: vi.fn(async () => ({ ids: ['a'], added: 1, existing: 0 }))
}));

vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: { listQueues, queueFromTraces, addQueueItems }
}));

async function open(props: Record<string, unknown>) {
	render(AddToQueue, { props } as never);
	const user = userEvent.setup({ delay: null });
	await user.click(screen.getByRole('button', { name: /Add to queue/ }));
	await screen.findByLabelText('Queue');
	return user;
}

beforeEach(() => {
	queueFromTraces.mockClear();
	addQueueItems.mockClear();
	document.body.style.pointerEvents = '';
});

describe('the filtered add', () => {
	it('names the count and the cap before the call', async () => {
		await open({
			filters: { status: 'error' },
			matched: '37 traces',
			blocked: null,
			label: 'Add to queue…'
		});

		expect(screen.getByText(/37 traces match these filters/)).toBeVisible();
		expect(screen.getByText(/at most 1,000 are added/)).toBeVisible();
		expect(screen.getByRole('button', { name: 'Add' })).toBeEnabled();
	});

	it('is disabled above the cap, with the reason on screen', async () => {
		await open({
			filters: { status: 'error' },
			matched: '1,000+ traces',
			blocked: 'More than 1,000 traces match. Narrow the filters.',
			label: 'Add to queue…'
		});

		expect(screen.getByText(/Narrow the filters/)).toBeVisible();
		expect(screen.getByRole('button', { name: 'Add' })).toBeDisabled();
		expect(queueFromTraces).not.toHaveBeenCalled();
	});

	it('sends the page filters to the queue that was picked', async () => {
		const user = await open({ filters: { status: 'error' }, matched: '37 traces' });
		await user.selectOptions(screen.getByLabelText('Queue'), 'errors');
		await user.click(screen.getByRole('button', { name: 'Add' }));

		await waitFor(() =>
			expect(queueFromTraces).toHaveBeenCalledWith('errors', { status: 'error' }, 1000)
		);
		expect(await screen.findByText(/37 added, 0 already there/)).toBeVisible();
	});
});

describe('the single target', () => {
	it('says whether it added the trace or found it already queued', async () => {
		const user = await open({ target: { trace_id: 't'.repeat(32) } });
		await user.click(screen.getByRole('button', { name: 'Add' }));

		await waitFor(() =>
			expect(addQueueItems).toHaveBeenCalledWith('weekly', { trace_id: 't'.repeat(32) })
		);
		expect(await screen.findByText('Added to weekly.')).toBeVisible();

		addQueueItems.mockResolvedValueOnce({ ids: ['a'], added: 0, existing: 1 });
		await user.click(screen.getByRole('button', { name: 'Add' }));
		expect(await screen.findByText('Already in weekly.')).toBeVisible();
	});
});

it('sends a project with no queues to make one', async () => {
	listQueues.mockResolvedValueOnce({ queues: [] });
	render(AddToQueue, { props: { target: { trace_id: 't'.repeat(32) } } } as never);
	await userEvent
		.setup({ delay: null })
		.click(screen.getByRole('button', { name: /Add to queue/ }));

	expect(await screen.findByRole('link', { name: 'Make one' })).toHaveAttribute('href', '/queues');
});
