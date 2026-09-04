import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ApiError } from '$lib/api/client.svelte';
import ConfirmDialog from './ConfirmDialog.svelte';

// The dialog behind the cheap deletions (spec 016 #6). Two things are the
// whole point of it: the consequence is named — for a run, that its traces are
// released rather than deleted — and a refusal leaves the reader inside the
// dialog with the server's own words, rather than closing over the failure.

function mount(onconfirm: () => Promise<void>) {
	render(ConfirmDialog, {
		open: true,
		title: 'Delete this run?',
		description: 'Its traces are not deleted — they return to the retention window.',
		confirmLabel: 'Delete the run',
		onconfirm,
		onclose: closed
	} as never);
	return userEvent.setup();
}

const closed = vi.fn();

describe('the confirmation dialog', () => {
	it('names the consequence and runs the act once', async () => {
		const act = vi.fn(async () => {});
		const user = mount(act);

		expect(screen.getByText(/return to the retention window/)).toBeInTheDocument();
		await user.click(screen.getByRole('button', { name: 'Delete the run' }));

		expect(act).toHaveBeenCalledTimes(1);
		expect(closed).toHaveBeenCalled();
	});

	it("keeps the question open on a refusal, in the server's words", async () => {
		const user = mount(async () => {
			throw new ApiError(409, 'run 0e5a is still running');
		});

		await user.click(screen.getByRole('button', { name: 'Delete the run' }));

		expect(await screen.findByRole('alert')).toHaveTextContent('run 0e5a is still running');
		expect(screen.getByRole('button', { name: 'Delete the run' })).toBeEnabled();
	});
});
