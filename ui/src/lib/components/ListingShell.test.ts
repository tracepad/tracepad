import { createRawSnippet } from 'svelte';
import { render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import type { Listing } from '$lib/listing.svelte';
import ListingCount from './ListingCount.svelte';
import ListingShell from './ListingShell.svelte';

// The ladder eleven listings used to carry (spec 026 #1), over a stubbed
// loader: each state, in the order the shell draws it. These are the
// assertions that used to exist as eleven copies of one `{#if}` and no test at
// all — spec 010's own count was five defects found in pairs, one file apart.

type Row = { id: string };

type Stub = Partial<{
	rows: Row[];
	loading: boolean;
	failure: string | null;
	problem: string | null;
	newest: boolean;
	total: { value: number; capped: boolean } | null;
}>;

/** A loader as the shell reads it: state and a bar to spread, nothing else. */
function stub(over: Stub = {}): Listing<Row> {
	const rows = over.rows ?? [];
	return {
		rows,
		loading: false,
		failure: null,
		problem: null,
		newest: true,
		total: null,
		...over,
		bar: {
			limit: 50,
			rows: rows.length,
			total: over.total ?? null,
			hasPrev: false,
			hasNext: false,
			busy: over.loading ?? false,
			atNewest: over.newest ?? true,
			atOldest: false,
			onresize: () => {},
			onfirst: () => {},
			onprev: () => {},
			onnext: () => {},
			onlast: () => {}
		}
	} as unknown as Listing<Row>;
}

const table = createRawSnippet(() => ({
	render: () => `<div data-testid="table">the rows</div>`
}));
const empty = createRawSnippet(() => ({
	render: () => `<div data-testid="empty">nothing here yet</div>`
}));

function mount(listing: Listing<Row>, extra: Record<string, unknown> = {}) {
	return render(ListingShell, {
		listing,
		noun: 'trace',
		table,
		empty,
		...extra
	} as never);
}

describe('the listing shell', () => {
	it('says what went wrong, above whatever else it draws', () => {
		mount(stub({ rows: [{ id: 'a' }], problem: 'Failed to read the traces.' }));

		expect(screen.getByRole('alert')).toHaveTextContent('Failed to read the traces.');
		expect(screen.getByTestId('table')).toBeInTheDocument();
	});

	it('draws the table and the bar for a page with rows', () => {
		mount(stub({ rows: [{ id: 'a' }, { id: 'b' }] }));

		expect(screen.getByTestId('table')).toBeInTheDocument();
		expect(screen.getByRole('button', { name: 'Next page' })).toBeInTheDocument();
		expect(screen.getByText('2 traces')).toBeInTheDocument();
		expect(screen.queryByTestId('empty')).not.toBeInTheDocument();
	});

	it('keeps the bar on an empty page that is not the newest, and says so', () => {
		mount(stub({ rows: [], newest: false }));

		// The way back from a cursor whose rows retention took: without the bar
		// there is none but editing the URL (PR #11 review).
		expect(screen.getByRole('button', { name: 'Newest page' })).toBeInTheDocument();
		expect(screen.getByText(/Nothing on this page any more/)).toHaveTextContent(
			'Nothing on this page any more. Use « to go back to the newest.'
		);
		expect(screen.queryByTestId('empty')).not.toBeInTheDocument();
	});

	it('says "the first" where a listing is not read newest first', () => {
		mount(stub({ rows: [], newest: false }), { back: 'first' });

		expect(screen.getByText(/Nothing on this page any more/)).toHaveTextContent(
			'Nothing on this page any more. Use « to go back to the first.'
		);
	});

	it('draws the empty state when the listing is idle and empty', () => {
		mount(stub());

		expect(screen.getByTestId('empty')).toBeInTheDocument();
		expect(screen.queryByTestId('table')).not.toBeInTheDocument();
	});

	it('draws nothing but the spacer while the first page is in flight', () => {
		const { container } = mount(stub({ loading: true }));

		expect(screen.queryByTestId('table')).not.toBeInTheDocument();
		expect(screen.queryByTestId('empty')).not.toBeInTheDocument();
		expect(screen.queryByRole('alert')).not.toBeInTheDocument();
		expect(container.querySelector('div.flex-1')).toBeInTheDocument();
	});

	it('draws no empty state over a failure', () => {
		mount(stub({ failure: 'Failed to read the traces.', problem: 'Failed to read the traces.' }));

		expect(screen.getByRole('alert')).toBeInTheDocument();
		expect(screen.queryByTestId('empty')).not.toBeInTheDocument();
	});

	it('takes a total the caller knows better than the loader does', () => {
		mount(stub({ rows: [{ id: 'a' }] }), { total: { value: 12, capped: false } });

		expect(screen.getByText('1 of 12 traces')).toBeInTheDocument();
	});

	it('renders the spacer for a consumer with no empty state of its own', () => {
		const { container } = render(ListingShell, {
			listing: stub(),
			noun: 'trace',
			table
		} as never);

		expect(container.querySelector('div.flex-1')).toBeInTheDocument();
	});
});

describe('the listing count', () => {
	it('spins while the page is in flight', () => {
		const { container } = render(ListingCount, { listing: stub({ loading: true }) } as never);

		expect(container.querySelector('.animate-spin')).toBeInTheDocument();
	});

	it('carries the cap of a capped total', () => {
		render(ListingCount, {
			listing: stub({ total: { value: 1000, capped: true } })
		} as never);

		expect(screen.getByText('1,000+')).toBeInTheDocument();
	});

	it('falls back to what is on screen, which is still a number', () => {
		render(ListingCount, {
			listing: stub({ rows: [{ id: 'a' }, { id: 'b' }, { id: 'c' }] })
		} as never);

		expect(screen.getByText('3')).toBeInTheDocument();
	});
});
