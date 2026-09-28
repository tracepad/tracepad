import { render, screen, within } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AnnotationItem } from '$lib/api/client.svelte';
import QueueItemTable from './QueueItemTable.svelte';
import { boxWidth } from '../../../tests/box';

// A queue's items at the two widths (spec 006 #18): on a phone the row is the
// target, its status and the two verbs, and who, when and why fold under the
// target — the reason on a line of its own, because it is the part worth
// reading and the end of a line is the part that gets cut.

vi.mock('$lib/project.svelte', () => ({ project: { editor: true } }));

let narrow = false;
beforeEach(() => {
	narrow = false;
	window.matchMedia = (query: string) =>
		({ matches: narrow, media: query, addEventListener() {}, removeEventListener() {} }) as never;
});

const SKIPPED = {
	id: 'item-2',
	seq: 2,
	trace_id: '78515700b088aabbccddeeff00112233',
	observation_id: null,
	status: 'skipped',
	completed_by: 'grace',
	completed_at: '2026-09-28T01:10:00Z',
	skip_reason: 'not a support conversation'
} as unknown as AnnotationItem;

const props = () => ({
	rows: [SKIPPED],
	onopen: vi.fn(),
	href: (id: string) => `/queues/q?peek=${id}`,
	onreopen: vi.fn(),
	onremove: vi.fn()
});

const heads = () => screen.getAllByRole('columnheader').map((one) => one.textContent?.trim());

describe('the queue item table', () => {
	it('has every column where there is room', () => {
		render(QueueItemTable, props());

		expect(heads()).toEqual(['#', 'Target', 'Status', 'By', 'When', 'Skip reason', 'Actions']);
		expect(screen.getByRole('button', { name: 'Reopen' })).toHaveTextContent('Reopen');
	});

	it('folds who, when and why under the target on a phone', () => {
		narrow = true;
		render(QueueItemTable, props());

		expect(heads()).toEqual(['Target', 'Status', 'Actions']);
		const target = screen.getByRole('link').closest('td')!;
		expect(within(target).getByText('#2 ·').parentElement).toHaveTextContent(/^#2 · grace · \S/);
		expect(within(target).getByText('not a support conversation')).toBeInTheDocument();
		expect(screen.getAllByText('not a support conversation')).toHaveLength(1);
	});

	it('keeps both verbs on a phone, named for whoever cannot see the icon', async () => {
		narrow = true;
		const given = props();
		render(QueueItemTable, given);

		const reopen = screen.getByRole('button', { name: 'Reopen' });
		expect(reopen).toHaveTextContent('');
		reopen.click();
		screen.getByRole('button', { name: 'Remove' }).click();
		expect(given.onreopen).toHaveBeenCalledWith(SKIPPED);
		expect(given.onremove).toHaveBeenCalledWith(SKIPPED);
		// The verbs are not a click on the row.
		expect(given.onopen).not.toHaveBeenCalled();
	});
});

describe('the queue item table by its box', () => {
	it('folds in a box narrower than the table, on a wide screen (#22)', () => {
		boxWidth(640);
		render(QueueItemTable, props());

		expect(heads()).toEqual(['Target', 'Status', 'Actions']);
		expect(screen.getByRole('button', { name: 'Reopen' })).not.toHaveTextContent('Reopen');
	});
});
