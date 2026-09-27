import { render, screen, within } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { TraceRow } from '$lib/api/client.svelte';
import TraceTable from './TraceTable.svelte';

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
		expect(screen.getByRole('table')).toHaveClass('min-w-3xl');
		expect(screen.getByRole('link', { name: 'user-1137' })).toBeInTheDocument();
	});

	it('keeps three columns on a phone and folds the rest under the name', () => {
		narrow = true;
		render(TraceTable, { rows: [ROW] });

		expect(heads()).toEqual(['Time', 'Name', 'Errors']);
		expect(screen.getByRole('table')).not.toHaveClass('min-w-3xl');
		const name = screen.getByText('answer-question').closest('td')!;
		expect(
			within(name).getByText('production · 2.08 s · TTFT 410 ms · $0.0054')
		).toBeInTheDocument();
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
