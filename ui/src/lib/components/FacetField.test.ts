import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { FILTER_BOX_FROM } from '$lib/api/facets';
import FacetField from './FacetField.svelte';

// The four states a facet list has to hold (spec 027 #6): the values with
// their counts, a checked value the range no longer holds, the read still in
// flight, and the read that failed. In the last two the checked values render
// alone, because they come from the URL rather than from the endpoint — and a
// filter nobody can see is a filter nobody can undo.

const values = [
	{ value: 'production', count: 4656 },
	{ value: 'staging', count: 12 },
	{ value: 'prod', count: 1 }
];

function draw(props: Partial<Parameters<typeof FacetField>[1]> = {}) {
	const onchange = vi.fn();
	render(FacetField, {
		id: 'filter-environment',
		label: 'Environment',
		name: 'environment',
		values,
		checked: [],
		onchange,
		...props
	});
	return onchange;
}

describe('the values', () => {
	it('draws each with its count, in the order the answer came', () => {
		draw();
		const boxes = screen.getAllByRole('checkbox');
		expect(boxes).toHaveLength(3);
		// The count is what tells a typo from the environment.
		expect(screen.getByText('4656')).toBeTruthy();
		expect(screen.getByText('1')).toBeTruthy();
	});

	it('reports the value a click added, and the one a click removed', async () => {
		const onchange = draw({ checked: ['staging'] });
		const user = userEvent.setup();

		await user.click(screen.getByRole('checkbox', { name: /production/ }));
		expect(onchange).toHaveBeenCalledWith(['staging', 'production']);

		await user.click(screen.getByRole('checkbox', { name: /staging/ }));
		expect(onchange).toHaveBeenLastCalledWith([]);
	});
});

describe('a value from a link', () => {
	// A URL is a document: `?environment=canary` from before canary was
	// retired must still show that it filters, and must still be undone.
	it('is checked at the top, without a count, and removable', async () => {
		const onchange = draw({ checked: ['canary'] });
		const box = screen.getByRole('checkbox', { name: /canary/ });
		expect((box as HTMLInputElement).checked).toBe(true);
		expect(screen.getAllByRole('checkbox')[0]).toBe(box);

		await userEvent.setup().click(box);
		expect(onchange).toHaveBeenCalledWith([]);
	});
});

describe('while the values are in flight', () => {
	it('renders the checked ones alone, and says it is loading', () => {
		draw({ values: [], checked: ['canary'], loading: true });
		expect(screen.getByRole('checkbox', { name: /canary/ })).toBeTruthy();
		expect(screen.getAllByRole('checkbox')).toHaveLength(1);
	});

	it('says so when there is nothing to show at all', () => {
		draw({ values: [], checked: [], loading: true });
		expect(screen.getByText('Loading…')).toBeTruthy();
	});
});

describe('when the read failed', () => {
	it('shows the failure line and keeps the checked values', () => {
		draw({ values: [], checked: ['canary'], failure: 'Failed to read the filter values.' });
		expect(screen.getByRole('alert').textContent).toContain('Failed to read the filter values.');
		expect(screen.getByRole('checkbox', { name: /canary/ })).toBeTruthy();
	});
});

describe('the filter box', () => {
	const many = Array.from({ length: FILTER_BOX_FROM + 1 }, (_, i) => ({
		value: `env-${i}`,
		count: 1
	}));

	it('is absent while the list is short enough to read', () => {
		draw();
		expect(screen.queryByRole('textbox')).toBeNull();
	});

	it('appears above eight values and narrows the list', async () => {
		draw({ values: many });
		const box = screen.getByRole('textbox');
		await userEvent.setup().type(box, 'env-1');
		expect(screen.getAllByRole('checkbox')).toHaveLength(1);
	});

	it('says when nothing matches, rather than showing an empty box', async () => {
		draw({ values: many });
		await userEvent.setup().type(screen.getByRole('textbox'), 'nope');
		expect(screen.getByText(/Nothing matches/)).toBeTruthy();
	});
});

describe('the cap', () => {
	it('says how many values it left out, so the list is not read as all of them', () => {
		draw({ omitted: 7 });
		expect(screen.getByText('and 7 more, too rare to list')).toBeTruthy();
	});

	it('says nothing when the list is the whole truth', () => {
		draw({ omitted: 0 });
		expect(screen.queryByText(/more, too rare/)).toBeNull();
	});
});
