import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import type { DatasetItem, SessionRow, UserRow } from '$lib/api/client.svelte';
import { boxWidth } from '../../tests/box';
import ItemTable from './evals/ItemTable.svelte';
import SessionTable from './SessionTable.svelte';
import UserTable from './UserTable.svelte';

// The project's listings in a box narrower than their tables (spec 006 #22):
// each keeps the columns that say which row it is and whether it went wrong,
// folds the rest under the row's name with what each value counts, and in a
// box as wide as the table is the table it always was.

vi.mock('$lib/project.svelte', () => ({ href: (path: string) => path }));


const heads = () => screen.getAllByRole('columnheader').map((one) => one.textContent?.trim());

const SESSION = {
	id: 'session-9',
	trace_count: 3,
	error_count: 1,
	total_cost: 0.0123,
	tokens: { input: 3000, output: 100 },
	first_seen: '2026-09-27T20:00:00Z',
	last_seen: '2026-09-27T21:00:00Z'
} as unknown as SessionRow;

const USER = {
	user_id: 'user-1137',
	traces: 1,
	sessions: 1,
	error_count: 0,
	total_cost: 2.5,
	tokens: { input: 900, output: 50, cache_read: 700 },
	first_seen: '2026-09-20T10:00:00Z',
	last_seen: '2026-09-27T21:00:00Z'
} as unknown as UserRow;

const ITEM = {
	id: 'a1b2c3d4e5f60718293a4b5c6d7e8f90',
	seq: 4,
	version: 2,
	input: { question: 'reset my password' },
	expected_output: { must_mention: ['settings'] }
} as unknown as DatasetItem;

describe('the sessions', () => {
	it('keep when, which and whether it failed, and fold the traces, the cost and the start', () => {
		boxWidth(390);
		render(SessionTable, { rows: [SESSION] });

		expect(heads()).toEqual(['Last seen', 'Session', 'Errors']);
		expect(screen.getByText('3 traces ·').parentElement).toHaveTextContent('3 traces · $0.0123 · 3.1k tokens');
		expect(screen.getByText(/^first seen /)).toBeInTheDocument();
		expect(screen.getByText('1 trace')).toBeInTheDocument();
	});

	it('are the whole table in a box as wide as it', () => {
		boxWidth(816);
		render(SessionTable, { rows: [SESSION] });

		expect(heads()).toHaveLength(7);
	});
});

describe('the users', () => {
	it('are the whole table in a box as wide as it, with the tokens and their classes', () => {
		boxWidth(944);
		render(UserTable, { rows: [USER] });

		expect(heads()).toHaveLength(8);
		expect(screen.getByRole('cell', { name: '950' })).toHaveAttribute(
			'title',
			'Input 900\nOutput 50\nCache read 700'
		);
	});

	it('keep who and whether it failed, and count in the singular where it is one', () => {
		boxWidth(390);
		render(UserTable, { rows: [USER] });

		expect(heads()).toEqual(['User', 'Errors']);
		expect(screen.getByText('1 trace ·').parentElement).toHaveTextContent('1 trace · 1 session · $2.50 · 950 tokens');
		expect(screen.getByText(/^last seen /)).toBeInTheDocument();
		expect(screen.getByRole('link', { name: 'user-1137' })).toHaveAttribute('href', '/users/user-1137');
	});
});

describe("a dataset's items", () => {
	it('keep the item and its input, and fold what it expects, its number and its version', () => {
		boxWidth(390);
		render(ItemTable, { rows: [ITEM], onopen: vi.fn(), href: (id: string) => `?peek=${id}` });

		expect(heads()).toEqual(['Id', 'Input']);
		expect(screen.getByText(/reset my password/)).toBeInTheDocument();
		expect(screen.getByText(/→ .*must_mention/)).toBeInTheDocument();
		expect(screen.getByText('#4 ·').parentElement).toHaveTextContent('#4 · v2');
	});
});
