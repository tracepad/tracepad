import { render, screen, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { boxWidth } from '../../../tests/box';
import ProjectsCard from './ProjectsCard.svelte';

// The Projects card at the two widths (spec 006 #24). Whole, it is five
// columns; in a box narrower than that the row is the name and its verbs, the
// id, retention and status fold under the name, and the buttons carry the name
// because the cell is not the row's header there (#18).

vi.mock('$app/state', () => ({ page: { url: new URL('http://tracepad.test/p/p1/settings/server') } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));

const PROJECTS = [
	{
		id: '0123456789abcdef0123456789abcdef',
		name: 'checkout',
		retention_days: 30,
		created_at: '2026-09-01T00:00:00Z',
		deleted_at: null,
		purge_at: null
	},
	{
		id: 'fedcba9876543210fedcba9876543210',
		name: 'legacy',
		retention_days: null,
		created_at: '2026-09-01T00:00:00Z',
		deleted_at: '2026-09-10T00:00:00Z',
		purge_at: '2026-10-10T00:00:00Z'
	}
];

vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: { listAllProjects: vi.fn(async () => ({ projects: PROJECTS })) }
}));

describe('the projects card', () => {
	it('folds the id, retention and status under the name, and names the verbs for it', async () => {
		boxWidth(400);
		render(ProjectsCard);

		const checkout = (await screen.findByText('checkout')).closest('tr')!;
		expect(screen.getAllByRole('columnheader').map((one) => one.textContent?.trim())).toEqual(['Name', 'Actions']);
		expect(checkout).toHaveTextContent('0123456789abcdef0123456789abcdef');
		expect(checkout).toHaveTextContent('30 days · Live');
		expect(within(checkout).getByRole('button', { name: 'Settings of checkout' })).toBeTruthy();
		expect(within(checkout).getByRole('button', { name: 'Delete checkout' })).toBeTruthy();

		const legacy = screen.getByText('legacy').closest('tr')!;
		expect(legacy).toHaveTextContent('Keep forever · Deleted, purged');
		expect(within(legacy).getByRole('button', { name: 'Restore legacy' })).toBeTruthy();
	});

	it('is the whole table in a box as wide as it', async () => {
		boxWidth(728);
		render(ProjectsCard);

		await screen.findByText('checkout');
		expect(screen.getAllByRole('columnheader')).toHaveLength(5);
		expect(screen.getAllByRole('button', { name: 'Delete' })).toHaveLength(1);
	});

	it('folds one rem under it', async () => {
		boxWidth(712);
		render(ProjectsCard);

		await screen.findByText('checkout');
		expect(screen.getAllByRole('columnheader')).toHaveLength(2);
	});
});
