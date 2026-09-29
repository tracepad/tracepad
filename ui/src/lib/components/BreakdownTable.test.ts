import { render, screen, within } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import { breakdown } from '$lib/api/stats';
import { boxWidth } from '../../tests/box';
import BreakdownTable from './BreakdownTable.svelte';

// The Tokens column (spec 031 #6): input plus output per row, and a dash —
// never a zero — for a row whose calls reported no usage.

describe('the breakdown table', () => {
	const rows = breakdown([
		{ key: 'claude-sonnet-5', count: 3, error_count: 0, latency_ms: {},
			tokens: { input: 1200, output: 34, cache_read: 500, reasoning: 20 } },
		{ key: 'local-llama', count: 1, error_count: 0, latency_ms: {} }
	]);

	it('shows input plus output per row, and a dash for none', () => {
		render(BreakdownTable, { title: 'By model', label: 'Model', unit: 'observation', rows, tokens: true });

		expect(screen.getByRole('columnheader', { name: 'Tokens' })).toBeInTheDocument();
		// The Tokens cell is the last one in a row; the bar sits behind the
		// number, so the number is the cell's text.
		const tokensOf = (model: string) =>
			within(screen.getByRole('row', { name: new RegExp(model) })).getAllByRole('cell').at(-1)!;
		expect(tokensOf('claude-sonnet-5')).toHaveTextContent(/^1,234$/);
		expect(tokensOf('local-llama')).toHaveTextContent(/^—$/);
	});

	it('lists every class the row reported in the cell tooltip, none added in', () => {
		render(BreakdownTable, { title: 'By model', label: 'Model', unit: 'observation', rows, tokens: true });

		const cell = within(screen.getByRole('row', { name: /claude-sonnet-5/ })).getAllByRole('cell').at(-1)!;
		// 1,234 is input plus output; reasoning is beside it, not in it (spec 049 #1).
		expect(cell).toHaveTextContent(/^1,234$/);
		expect(cell).toHaveAttribute('title', 'Input 1,200\nOutput 34\nCache read 500\nReasoning 20');
		const none = within(screen.getByRole('row', { name: /local-llama/ })).getAllByRole('cell').at(-1)!;
		expect(none).not.toHaveAttribute('title');
	});

	it('has no Tokens column unless asked, for the screens whose answer carries none', () => {
		render(BreakdownTable, { title: 'By model', label: 'Model', unit: 'observation', rows });

		expect(screen.queryByRole('columnheader', { name: 'Tokens' })).not.toBeInTheDocument();
		expect(within(screen.getByRole('row', { name: /claude-sonnet-5/ })).getAllByRole('cell')).toHaveLength(3);
	});

	// In a box narrower than the table (spec 006 #24): the key, how many and
	// how many failed stay columns with their bars; the cost and the tokens
	// fold under the key.
	describe('in a narrow box', () => {
		const heads = () => screen.getAllByRole('columnheader').map((one) => one.textContent?.trim());

		it('keeps the key, the count and the errors, and folds the tokens under the key', () => {
			boxWidth(356);
			render(BreakdownTable, { title: 'By model', label: 'Model', unit: 'observation', rows, tokens: true });

			expect(heads()).toEqual(['Model', 'Observations', 'Errors']);
			const row = screen.getByRole('row', { name: /claude-sonnet-5/ });
			expect(within(row).getAllByRole('cell').map((one) => one.textContent?.trim())).toEqual([
				'claude-sonnet-5 1,234 tokens',
				'3',
				'0'
			]);
		});

		it('is the whole table in a box as wide as it: 480 px, or 400 without the tokens', () => {
			boxWidth(480);
			const { unmount } = render(BreakdownTable, { title: 'By model', label: 'Model', unit: 'observation', rows, tokens: true });
			expect(heads()).toEqual(['Model', 'Observations', 'Errors', 'Cost', 'Tokens']);
			unmount();

			boxWidth(400);
			render(BreakdownTable, { title: 'By model', label: 'Model', unit: 'observation', rows });
			expect(heads()).toEqual(['Model', 'Observations', 'Errors', 'Cost']);
		});
	});
});
