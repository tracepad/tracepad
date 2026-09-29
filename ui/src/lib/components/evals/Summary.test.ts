import { render, screen, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import type { RunWithSummary } from '$lib/api/client.svelte';
import { boxWidth } from '../../../tests/box';
import Summary from './Summary.svelte';

// A run's scores in a box narrower than their table (spec 006 #24): a score
// keeps its name and what it came to, and its type, how many and the mean
// fold under the name; in a box as wide as the table it is the table.

vi.mock('$lib/project.svelte', () => ({ href: (path: string) => path }));


const SUMMARY = {
	items: { total: 3, covered: 2, missing: 1, unknown: 0 },
	traces: { count: 2, attempts_max: 1, error_count: 0, total_cost: null, latency_ms: {} },
	models: [],
	prompts: [],
	scores: {
		accuracy: { data_type: 'numeric', direction: 'higher', count: 12, mean: 0.5594, min: 0, max: 0.963 },
		tone: { data_type: 'categorical', direction: null, count: 1, distribution: { friendly: 1 } }
	}
} as unknown as RunWithSummary['summary'];

const scores = () => screen.getByRole('heading', { name: 'Scores' }).parentElement!;
const heads = () =>
	within(scores())
		.getAllByRole('columnheader')
		.map((one) => one.textContent?.trim());

describe("a run's scores", () => {
	it('keep the name and the range, and fold the type, the count and the mean', () => {
		boxWidth(356);
		render(Summary, { summary: SUMMARY, metadata: {} });

		expect(heads()).toEqual(['Name', 'Range or distribution']);
		const accuracy = within(scores()).getByText('accuracy').closest('tr')!;
		expect(accuracy).toHaveTextContent('numeric · higher · 12 scores · mean 0.5594');
		expect(accuracy).toHaveTextContent('0 … 0.963');
		// One score, and no mean to fold for a categorical name.
		const tone = within(scores()).getByText('tone').closest('tr')!;
		expect(tone).toHaveTextContent('categorical · 1 score');
		expect(tone).not.toHaveTextContent(/scores|mean/);
	});

	it('are the whole table in a box as wide as it', () => {
		boxWidth(496);
		render(Summary, { summary: SUMMARY, metadata: {} });

		expect(heads()).toEqual(['Name', 'Type', 'Count', 'Mean', 'Range or distribution']);
	});
});
