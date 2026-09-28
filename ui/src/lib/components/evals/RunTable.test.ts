import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Run } from '$lib/api/client.svelte';
import { boxWidth } from '../../../tests/box';
import RunTable from './RunTable.svelte';

// The checkboxes that are the second way into a comparison (spec 016 #11).
// The rule itself is `compareChoice`'s and tested in `evals.test.ts`; what is
// tested here is what the table ticks *with* — the rows on screen, and only
// those.

const run = (id: string, dataset = 'golden'): Run => ({
	id,
	dataset,
	dataset_version: 1,
	name: `run ${id}`,
	metadata: null,
	status: 'finished',
	error: null,
	created_at: '2026-09-04T10:00:00Z',
	finished_at: '2026-09-04T10:05:00Z'
});

const a = run('a'.repeat(32));
const b = run('b'.repeat(32));

describe('ticking two runs', () => {
	it('turns Compare into a link to the pair', async () => {
		const user = userEvent.setup();
		render(RunTable, { rows: [a, b] } as never);

		for (const box of screen.getAllByRole('checkbox')) await user.click(box);

		expect(screen.getByRole('link', { name: 'Compare' })).toHaveAttribute(
			'href',
			`/runs/${a.id}/compare/${b.id}`
		);
	});

	// A tick outlives the page it was made on: the state is the table's, and a
	// page turn or a filter change replaces the rows under it. Comparing a run
	// the reader can no longer see would be a comparison nobody asked for
	// (found in review of PR #33).
	it('forgets a run the listing no longer holds', async () => {
		const user = userEvent.setup();
		const { rerender } = render(RunTable, { rows: [a, b] } as never);
		for (const box of screen.getAllByRole('checkbox')) await user.click(box);
		expect(screen.getByRole('link', { name: 'Compare' })).toBeInTheDocument();

		await rerender({ rows: [a] } as never);

		expect(screen.queryByRole('link', { name: 'Compare' })).toBeNull();
		expect(screen.getByRole('button', { name: 'Compare' })).toBeDisabled();
		expect(screen.getByText('Tick two runs to compare them')).toBeInTheDocument();
		// The one still on screen keeps its tick, so the reader has one to go on.
		expect(screen.getByRole('checkbox')).toBeChecked();
	});
});

// Spec 006 #22: in a box narrower than the table a run is its tick, its name
// and how it stands; the dataset, still a link, the version and when it ran
// fold under the name, and the name is the row's link.
describe('in a narrow box', () => {
	it('keeps the tick, the name and the status, and folds the rest under the name', () => {
		boxWidth(390);
		render(RunTable, { rows: [a], withDataset: true } as never);

		const heads = screen.getAllByRole('columnheader').map((one) => one.textContent?.trim());
		expect(heads).toEqual(['Compare', 'Name', 'Status']);
		expect(screen.getByRole('link', { name: `run ${a.id}` })).toHaveAttribute('href', `/runs/${a.id}`);
		expect(screen.getByRole('link', { name: 'golden' })).toHaveAttribute('href', '/datasets/golden');
		expect(screen.getByText(/^v1 ·/).parentElement).toHaveTextContent(/^v1 · \S.* · finished \S/);
		expect(screen.getAllByRole('checkbox')).toHaveLength(1);
	});

	it('keeps every column in a box as wide as the table', () => {
		boxWidth(896);
		render(RunTable, { rows: [a], withDataset: true } as never);

		expect(screen.getAllByRole('columnheader')).toHaveLength(7);
	});

	// The runs of one dataset have no Dataset column, so their table is narrower
	// and keeps its columns in a narrower box.
	it('folds at 720 px without the Dataset column, not at 896', () => {
		boxWidth(720);
		const { unmount } = render(RunTable, { rows: [a] } as never);
		expect(screen.getAllByRole('columnheader')).toHaveLength(6);
		unmount();

		boxWidth(719);
		render(RunTable, { rows: [a] } as never);
		expect(screen.getAllByRole('columnheader')).toHaveLength(3);
	});
});
