import { render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { TraceRow } from '$lib/api/client.svelte';
import TraceTable from './TraceTable.svelte';
import { boxWidth } from '../../tests/box';
import { tooltipOver } from '../../tests/tooltip';

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
	tokens: { input: 12_000, output: 400, reasoning: 90, cache_write: 5 },
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
			'Tokens',
			'Latency',
			'TTFT',
			'Errors'
		]);
		// Declared in rem, so it scales with the reader's root size (spec 006
		// #22), and computed to pixels at the root's 16 px, as a browser
		// computes it at the reader's.
		const table = screen.getByRole('table');
		expect(table.style.minWidth).toBe('62rem');
		expect(getComputedStyle(table).minWidth).toBe('992px');
		expect(screen.getByRole('link', { name: 'user-1137' })).toBeInTheDocument();
	});

	it('shows input plus output compactly, with every class reported in the tooltip', () => {
		render(TraceTable, { rows: [ROW, { ...ROW, id: 'bb', tokens: undefined }] });

		const cell = screen.getByRole('cell', { name: '12.4k' });
		expect(cell).toHaveAttribute('title', 'Input 12,000\nOutput 400\nReasoning 90\nCache write 5');
		// No tokens is a dash and no tooltip, not a zero.
		expect(screen.getAllByRole('cell', { name: '—' })[0]).not.toHaveAttribute('title');
	});

	it('gives the folded tokens the class breakdown as their tooltip', () => {
		narrow = true;
		render(TraceTable, { rows: [ROW] });

		expect(tooltipOver(screen.getByText(/12\.4k tokens/))).toBe(
			'Input 12,000\nOutput 400\nReasoning 90\nCache write 5'
		);
		// The rest of the line keeps repeating itself for the sake of a cut value.
		expect(tooltipOver(screen.getByText(/^\$0\.0054/))).toBe('$0.0054');
	});

	it('keeps three columns on a phone and folds the rest under the name', () => {
		narrow = true;
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toEqual(['Time', 'Name', 'Errors']);
		expect(screen.getByRole('table').style.minWidth).toBe('');
		const name = screen.getByText('answer-question').closest('td')!;
		// One value to a box, so a line breaks between values and never inside one.
		const line = within(name).getByText(/^production/).parentElement!;
		expect(line).toHaveTextContent('production · 2.08 s · TTFT 410 ms · $0.0054 · 12.4k tokens');
		expect([...line.querySelectorAll('span')].map((one) => one.textContent)).toEqual([
			'production ·',
			'2.08 s ·',
			'TTFT 410 ms ·',
			'$0.0054 ·',
			'12.4k tokens'
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
		boxWidth(991);
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toEqual(['Time', 'Name', 'Errors']);
		expect(screen.getByRole('table').style.minWidth).toBe('');
	});

	it('keeps every column in a box as wide as the table, whatever the screen', () => {
		narrow = true;
		boxWidth(992);
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toHaveLength(10);
		const table = screen.getByRole('table');
		expect(table.style.minWidth).toBe('62rem');
		expect(getComputedStyle(table).minWidth).toBe('992px');
	});

	// The table's columns are rem, so its width is: a reader whose default is
	// 20 px has a table 1,240 px wide, and a 1,000 px box folds it.
	it('folds at the table\'s width in rem, for a reader with a larger default size', () => {
		document.documentElement.style.fontSize = '20px';
		boxWidth(1000);
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toEqual(['Time', 'Name', 'Errors']);
	});

	it('is as wide as its rem at the reader\'s size, when the box has room for it', () => {
		document.documentElement.style.fontSize = '20px';
		boxWidth(1240);
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toHaveLength(10);
		expect(getComputedStyle(screen.getByRole('table')).minWidth).toBe('1240px');
	});

	it('folds and unfolds as its box is resized after it is on the screen', async () => {
		boxWidth(1200);
		render(TraceTable, { rows: [ROW] });
		expect(heads()).toHaveLength(10);

		boxWidth(600);
		await tick();
		expect(heads()).toEqual(['Time', 'Name', 'Errors']);

		boxWidth(1200);
		await tick();
		expect(heads()).toHaveLength(10);
	});
});
