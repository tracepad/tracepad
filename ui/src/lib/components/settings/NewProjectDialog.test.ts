import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import NewProjectDialog from './NewProjectDialog.svelte';

// A project is a name, and its first key pair comes back with it — once
// (spec 007 #3, spec 029 #7). The order of what follows is the point here:
// the keys go on screen before `me` is read again, so that a read that fails
// cannot take the one showing of the secret with it.

const createProject = vi.fn();
const refresh = vi.fn();

vi.mock('$app/state', () => ({ page: { url: new URL('http://tracepad.test/p/x/settings/server') } }));
vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: { createProject: (...args: unknown[]) => createProject(...args) }
}));
vi.mock('$lib/session', () => ({ refresh: () => refresh() }));

const MADE = {
	id: 'c'.repeat(32),
	name: 'staging',
	retention_days: null,
	raw_retention_days: null,
	stats_retention_days: null,
	created_at: '2026-09-11T10:00:00Z',
	public_key: 'tp-pk-new',
	secret_key: 'tp-sk-new'
};

beforeEach(() => {
	createProject.mockReset();
	refresh.mockReset();
	// The dialog moves focus into itself a frame after opening; jsdom's frame
	// is a timer that can land mid-typing. A frame that is a zero timer has
	// fired one turn of the clock after the render (as ProjectSwitcher.test).
	vi.stubGlobal('requestAnimationFrame', (frame: FrameRequestCallback) =>
		setTimeout(() => frame(performance.now()), 0)
	);
});

afterEach(() => {
	vi.unstubAllGlobals();
});

async function submit() {
	const user = userEvent.setup();
	const oncreated = vi.fn();
	const onclose = vi.fn();
	render(NewProjectDialog, { props: { open: true, onclose, oncreated } });
	await new Promise((done) => setTimeout(done, 0));
	await user.type(screen.getByLabelText('Name'), 'staging');
	await user.click(screen.getByRole('button', { name: 'Create' }));
	return { user, oncreated, onclose };
}

describe('creating a project', () => {
	it('shows the keys, reads `me` again, and then tells the caller', async () => {
		createProject.mockResolvedValue(MADE);
		refresh.mockResolvedValue(undefined);
		const { oncreated } = await submit();

		await waitFor(() => expect(screen.getByText(/LANGFUSE_SECRET_KEY=tp-sk-new/)).toBeVisible());
		expect(createProject).toHaveBeenCalledWith('staging');
		expect(refresh).toHaveBeenCalledOnce();
		expect(oncreated).toHaveBeenCalledWith(MADE);
	});

	it('still shows the keys when `me` cannot be read, and tells the caller once it can', async () => {
		createProject.mockResolvedValue(MADE);
		let again!: () => void;
		refresh
			.mockRejectedValueOnce(new Error('network'))
			.mockReturnValueOnce(new Promise<void>((resolve) => (again = resolve)));
		const { user, oncreated, onclose } = await submit();

		await waitFor(() => expect(screen.getByText(/LANGFUSE_SECRET_KEY=tp-sk-new/)).toBeVisible());
		expect(oncreated).not.toHaveBeenCalled();
		expect(screen.queryByRole('alert')).toBeNull();

		// While `me` is read again the form stays shut: a Create with the
		// same name in it would mint the project twice.
		await user.click(screen.getByRole('button', { name: 'I have copied it' }));
		await waitFor(() => expect(refresh).toHaveBeenCalledTimes(2));
		expect(screen.queryByRole('button', { name: 'Create' })).toBeNull();
		expect(oncreated).not.toHaveBeenCalled();

		again();
		await waitFor(() => expect(oncreated).toHaveBeenCalledWith(MADE));
		expect(refresh).toHaveBeenCalledTimes(2);
		expect(onclose).toHaveBeenCalledOnce();
	});

	it('says so when the project could not be made, and shows no keys', async () => {
		createProject.mockRejectedValue(new Error('boom'));
		const { oncreated } = await submit();

		await waitFor(() =>
			expect(screen.getByRole('alert')).toHaveTextContent('Failed to create the project.')
		);
		expect(refresh).not.toHaveBeenCalled();
		expect(oncreated).not.toHaveBeenCalled();
		expect(screen.queryByText(/LANGFUSE_SECRET_KEY/)).toBeNull();
	});
});
