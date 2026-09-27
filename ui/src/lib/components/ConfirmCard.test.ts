import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ApiError, type DryRun } from '$lib/api/client.svelte';
import ConfirmCard from './ConfirmCard.svelte';

/**
 * The destructive card (spec 007 #5). Three things have to hold, and all three
 * are the difference between a speed bump and a decoration: the preview comes
 * from the server, the execute button stays shut until the echo matches, and a
 * refusal is reported in the server's own words.
 */

const PLAN: DryRun = {
	dry_run: true,
	would_delete: { traces: 412, observations: 1900 },
	oldest: '2026-08-01T09:00:00Z',
	confirm: 'my-project',
	note: 'the shorter window takes effect on the next sweep'
};

function mount(over: Partial<Parameters<typeof ConfirmCard>[1]> = {}) {
	const execute = vi.fn(async () => 'Done.');
	const props = {
		title: 'Shorten retention',
		description: 'It deletes what falls outside the new window.',
		echoLabel: 'project name',
		executeLabel: 'Shorten and delete',
		preview: async () => PLAN,
		execute,
		...over
	};
	const { rerender } = render(ConfirmCard, props as never);
	return { execute, rerender, props, user: userEvent.setup() };
}

describe('the confirm card', () => {
	it("renders the server's numbers rather than any of its own", async () => {
		const { user } = mount();

		await user.click(screen.getByRole('button', { name: 'Preview' }));

		// Exactly what the dry run said, including the note it chose to add.
		expect(await screen.findByText('412')).toBeInTheDocument();
		expect(screen.getByText('1,900')).toBeInTheDocument();
		expect(screen.getByText(/next sweep/)).toBeInTheDocument();
		expect(screen.getByText(/Reaching back to/)).toBeInTheDocument();
	});

	it('names the datasets an erasure takes items from, beside the runs', async () => {
		const { user } = mount({
			preview: async () => ({
				...PLAN,
				would_delete: { traces: 2, dataset_items: 3 },
				affected_datasets: [
					{ dataset: 'golden', items: 2 },
					{ dataset: 'edge-cases', items: 1 }
				]
			})
		});

		await user.click(screen.getByRole('button', { name: 'Preview' }));

		expect(await screen.findByText(/Datasets lose the items cut from these/)).toBeInTheDocument();
		expect(screen.getByText('golden (2)')).toBeInTheDocument();
		expect(screen.getByText('edge-cases (1)')).toBeInTheDocument();
	});

	it('keeps the button shut until the echo is exact', async () => {
		const { execute, user } = mount();
		await user.click(screen.getByRole('button', { name: 'Preview' }));

		const button = await screen.findByRole('button', { name: 'Shorten and delete' });
		expect(button).toBeDisabled();

		const echo = screen.getByRole('textbox');
		await user.type(echo, 'my-projec');
		expect(button).toBeDisabled();

		await user.type(echo, 't');
		expect(button).toBeEnabled();

		await user.click(button);
		expect(execute).toHaveBeenCalledWith('my-project');
	});

	it('will not be talked into a near miss', async () => {
		const { user } = mount();
		await user.click(screen.getByRole('button', { name: 'Preview' }));
		await user.type(await screen.findByRole('textbox'), 'My-Project');

		// Case included: the server compares the string it handed back.
		expect(screen.getByRole('button', { name: 'Shorten and delete' })).toBeDisabled();
	});

	it("reports a refusal in the server's own words", async () => {
		const { user } = mount({
			preview: async () => {
				throw new ApiError(400, 'confirm must be the project name, got "nope"');
			}
		});

		await user.click(screen.getByRole('button', { name: 'Preview' }));

		expect(await screen.findByRole('alert')).toHaveTextContent(
			'confirm must be the project name'
		);
	});

	it('drops a plan once it stops describing what is on screen', async () => {
		// The hole this closes: the echo the server asks for names the project,
		// not the change, so a plan left standing across an edit would let
		// "delete 412 traces" be confirmed into a window that deletes far more.
		const { rerender, props, user } = mount({ subject: { retention_days: 30 } });

		await user.click(screen.getByRole('button', { name: 'Preview' }));
		expect(await screen.findByRole('textbox')).toBeInTheDocument();

		await rerender({ ...props, subject: { retention_days: 1 } } as never);

		// Back to the preview button: this change has not been priced yet.
		expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
		expect(screen.getByRole('button', { name: 'Preview' })).toBeInTheDocument();
	});

	it('will not ask the server about a card that has nothing in it yet', async () => {
		mount({ ready: false });

		expect(screen.getByRole('button', { name: 'Preview' })).toBeDisabled();
	});

	it('says so when there was nothing to confirm', async () => {
		// A retention window that grew, or a key that was not the last one: the
		// server did it on the first call, and the card must not pretend to be
		// waiting for an echo it will never get.
		const { user } = mount({ preview: async () => 'Retention updated.' });

		await user.click(screen.getByRole('button', { name: 'Preview' }));

		expect(await screen.findByRole('status')).toHaveTextContent('Retention updated.');
		expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
	});
});
