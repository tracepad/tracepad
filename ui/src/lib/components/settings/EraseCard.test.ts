import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import EraseCard from './EraseCard.svelte';

// The erase card follows the erasure it started while it runs (spec 047 #18),
// and that erasure is its project's: another project's card starts with none,
// while the same project read again keeps it (#29).

vi.mock('$app/state', () => ({ page: { url: new URL('http://tracepad.test/p/p1/settings') } }));

const RUNNING = {
	id: '4f0c9d3e8a1b2c3d4e5f60718293a4b5',
	state: 'running',
	phase: 'parsed',
	user_id: 'user-4711',
	dry_run: false,
	created_at: '2026-10-02T09:00:00Z',
	started_at: '2026-10-02T09:00:00Z',
	finished_at: null,
	progress: { traces_at_start: 20000, traces_deleted: 5123 },
	deleted: { traces: 5123 },
	compaction: { requested_at: null, expected_by: null },
	error: null
};

const eraseUserData = vi.fn(async (_project: string, user: string, confirm?: string) =>
	confirm === undefined
		? { dry_run: true, would_delete: { traces: 20000 }, confirm: user }
		: RUNNING
);
const erasures = vi.fn(async () => ({ erasures: [] }));

vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		eraseUserData: (...args: unknown[]) =>
			eraseUserData(...(args as [string, string, string | undefined])),
		erasures: (...args: unknown[]) => erasures(...(args as [])),
		erasure: vi.fn(() => new Promise(() => {}))
	}
}));

beforeEach(() => {
	eraseUserData.mockClear();
	erasures.mockClear();
});

async function startErasure() {
	const user = userEvent.setup();
	const card = render(EraseCard, { current: { id: 'p1', name: 'checkout' } } as never);
	await user.type(screen.getByLabelText('User id'), 'user-4711');
	await user.click(screen.getByRole('button', { name: 'Show what would go' }));
	await user.type(await screen.findByRole('textbox', { name: /Type the user id/ }), 'user-4711');
	await user.click(screen.getByRole('button', { name: "Erase this user's data" }));
	expect(await screen.findByTestId('erasure-progress')).toHaveTextContent(
		'Erasure in progress — parsed, 5,123 of 20,000 traces'
	);
	return card;
}

describe('the erase card', () => {
	it("does not show one project's erasure on another's card", async () => {
		const { rerender } = await startErasure();

		await rerender({ current: { id: 'p2', name: 'search' } } as never);

		expect(screen.queryByTestId('erasure-progress')).toBeNull();
		expect(erasures).toHaveBeenLastCalledWith('p2');
	});

	it("leaves an answer alone that came back after the card's project changed", async () => {
		const user = userEvent.setup();
		let answer: (value: unknown) => void = () => {};
		eraseUserData.mockImplementationOnce(async (_project, who) => ({
			dry_run: true,
			would_delete: { traces: 1 },
			confirm: who
		}));
		eraseUserData.mockImplementationOnce(
			() => new Promise((resolve) => (answer = resolve)) as never
		);
		const { rerender } = render(EraseCard, { current: { id: 'p1', name: 'checkout' } } as never);
		await user.type(screen.getByLabelText('User id'), 'user-4711');
		await user.click(screen.getByRole('button', { name: 'Show what would go' }));
		await user.type(await screen.findByRole('textbox', { name: /Type the user id/ }), 'user-4711');
		await user.click(screen.getByRole('button', { name: "Erase this user's data" }));

		await rerender({ current: { id: 'p2', name: 'search' } } as never);
		answer(RUNNING);

		expect(await screen.findByText(/runs on the server\.$/)).toBeInTheDocument();
		expect(screen.queryByTestId('erasure-progress')).toBeNull();
	});

	it('keeps following it when its project is read again', async () => {
		const { rerender } = await startErasure();

		await rerender({ current: { id: 'p1', name: 'checkout, renamed' } } as never);

		expect(screen.getByTestId('erasure-progress')).toHaveTextContent('Erasure in progress');
	});
});
