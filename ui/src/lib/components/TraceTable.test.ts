import { render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { TraceRow } from '$lib/api/client.svelte';
import TraceTable from './TraceTable.svelte';
import { boxWidth } from '../../tests/box';

// The trace listing at the two widths it has (spec 006 #18): every column on a
// screen with room for them, and on a phone the three that say when, what and
// whether it failed, with the rest folded under the name. One layout is in the
// document at a time, so nothing is there twice for a search or a screen
// reader to find.

vi.mock('$lib/project.svelte', () => ({ href: (path: string) => path }));

let narrow = false;
beforeEach(() => {
	narrow = false;
	window.matchMedia = (query: string) =>
		({ matches: narrow, media: query, addEventListener() {}, removeEventListener() {} }) as never;
});

const ROW = {
	id: 'aa11bb22cc33dd44ee55ff6600112233',
	timestamp: '2026-09-27T20:36:22Z',
	name: 'answer-question',
	environment: 'production',
	user_id: 'user-1137',
	session_id: 'session-9',
	total_cost: 0.0054,
	latency_ms: 2080,
	ttft_ms: 410,
	error_count: 2
} as unknown as TraceRow;

const heads = () => screen.getAllByRole('columnheader').map((one) => one.textContent?.trim());

describe('the trace table', () => {
	it('has every column where there is room', () => {
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toEqual([
			'Time',
			'Name',
			'Environment',
			'User',
			'Session',
			'Cost',
			'Latency',
			'TTFT',
			'Errors'
		]);
		expect(screen.getByRole('table')).toHaveStyle({ minWidth: '56rem' });
		expect(screen.getByRole('link', { name: 'user-1137' })).toBeInTheDocument();
	});

	it('keeps three columns on a phone and folds the rest under the name', () => {
		narrow = true;
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toEqual(['Time', 'Name', 'Errors']);
		expect(screen.getByRole('table').style.minWidth).toBe('');
		const name = screen.getByText('answer-question').closest('td')!;
		// One value to a box, so a line breaks between values and never inside one.
		const line = within(name).getByText(/^production/).parentElement!;
		expect(line).toHaveTextContent('production · 2.08 s · TTFT 410 ms · $0.0054');
		expect([...line.querySelectorAll('span')].map((one) => one.textContent)).toEqual([
			'production ·',
			'2.08 s ·',
			'TTFT 410 ms ·',
			'$0.0054'
		]);
		// The failure is still in words, not a colour on its own.
		expect(screen.getByText('2 errors')).toBeInTheDocument();
		// The user and the session are still destinations, once each.
		expect(within(name).getByRole('link', { name: 'user-1137' })).toBeInTheDocument();
		expect(within(name).getByRole('link', { name: 'session-9' })).toBeInTheDocument();
		expect(screen.getAllByText('user-1137')).toHaveLength(1);
		expect(screen.getAllByText(/production/)).toHaveLength(1);
	});

	it('leaves the session a text on a phone inside that session', () => {
		narrow = true;
		render(TraceTable, { rows: [{ ...ROW, user_id: undefined }], linkSession: false });

		expect(screen.queryByRole('link', { name: 'session-9' })).not.toBeInTheDocument();
		expect(screen.getByText('session-9')).toBeInTheDocument();
	});

	it('spans a search match across the columns there are', () => {
		narrow = true;
		const matched = {
			...ROW,
			match: { field: 'input', snippet: 'reset my password', observation_id: null }
		} as unknown as TraceRow;
		render(TraceTable, { rows: [matched], search: 'password' });

		expect(screen.getByText('input').closest('td')).toHaveAttribute('colspan', '3');
	});
});

// The width is the box's, not the screen's (#22): a desktop listing beside
// the sidebar or inside the peek panel can be narrower than its table, and
// folds the same.
describe('the trace table by its box', () => {
	// Whatever the test did to the root, the next one finds it as it was.
	afterEach(() => {
		document.documentElement.style.fontSize = '';
	});

	it('folds in a box narrower than the table, on a wide screen', () => {
		boxWidth(895);
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toEqual(['Time', 'Name', 'Errors']);
		expect(screen.getByRole('table').style.minWidth).toBe('');
	});

	it('keeps every column in a box as wide as the table, whatever the screen', () => {
		narrow = true;
		boxWidth(896);
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toHaveLength(9);
		expect(screen.getByRole('table')).toHaveStyle({ minWidth: '56rem' });
	});

	// The table's columns are rem, so its width is: a reader whose default is
	// 20 px has a table 1,120 px wide, and a 1,000 px box folds it.
	it('folds at the table\'s width in rem, for a reader with a larger default size', () => {
		document.documentElement.style.fontSize = '20px';
		boxWidth(1000);
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toEqual(['Time', 'Name', 'Errors']);
	});

	it('folds and unfolds as its box is resized after it is on the screen', async () => {
		boxWidth(1200);
		render(TraceTable, { rows: [ROW] });
		expect(heads()).toHaveLength(9);

		boxWidth(600);
		await tick();
		expect(heads()).toEqual(['Time', 'Name', 'Errors']);

		boxWidth(1200);
		await tick();
		expect(heads()).toHaveLength(9);
	});
});
