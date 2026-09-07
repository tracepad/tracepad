import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { ScoreConfig } from '$lib/api/client.svelte';
import { emptyScoreForm, type ScoreForm } from '$lib/scores';
import ScoreControl from './ScoreControl.svelte';

// The control the config dictates, tested where it now lives (spec 024 #12).
// These are the dialog's own tests, moved: what they check is the rule that
// the config — not the component asking — decides what a value looks like, and
// the desk builds the same control from the same rule.

const config = (extra: Partial<ScoreConfig>): ScoreConfig => ({
	name: 'accuracy',
	data_type: 'numeric',
	direction: 'higher',
	min: 0,
	max: 1,
	categories: null,
	description: null,
	created_at: '2026-09-07T10:00:00Z',
	updated_at: '2026-09-07T10:00:00Z',
	...extra
});

function control(type: string, one?: ScoreConfig, form: ScoreForm = emptyScoreForm()) {
	render(ScoreControl, { props: { type, config: one, form } } as never);
	return userEvent.setup({ delay: null });
}

describe('the control the config dictates', () => {
	it('gives a bounded number field to a numeric name', () => {
		control('numeric', config({}));

		const value = screen.getByLabelText('Value');
		expect(value).toHaveAttribute('type', 'number');
		expect(value).toHaveAttribute('min', '0');
		expect(value).toHaveAttribute('max', '1');
		expect(screen.getByText('0 … 1')).toBeVisible();
	});

	it('gives a categorical name its categories and nothing else', () => {
		control('categorical', config({ data_type: 'categorical', categories: ['correct', 'wrong'] }));

		const value = screen.getByLabelText('Value');
		expect([...value.querySelectorAll('option')].map((one) => one.value)).toEqual([
			'',
			'correct',
			'wrong'
		]);
	});

	// A boolean's value is two buttons, so there is no input for a `for` to
	// point at — the group borrows the label instead of leaving it dangling
	// (found in review of PR #41).
	it('labels the boolean value group with the label above it', () => {
		control('boolean', config({ data_type: 'boolean', min: null, max: null }));

		const group = screen.getByRole('group', { name: 'Value' });
		expect(group).toContainElement(screen.getByRole('button', { name: 'yes' }));
		expect(document.querySelector('label[for]')).toBeNull();
	});

	// The desk draws one of these per score the queue asks for, so the label
	// is the score's name and not the word *Value* three times over (#12).
	it('takes the name of what is being scored as its label', () => {
		render(ScoreControl, {
			props: { type: 'text', form: emptyScoreForm(), label: 'tone' }
		} as never);

		expect(screen.getByLabelText('tone')).toBeVisible();
	});

	// Nothing is drawn until the type is known: on the free-name path of the
	// dialog that is what the radio buttons are for.
	it('draws nothing while the type is unknown', () => {
		control('');

		expect(screen.queryByLabelText('Value')).toBeNull();
	});

	it('writes what is typed back into the form it was handed', async () => {
		const form = emptyScoreForm();
		const user = control('numeric', config({}), form);
		await user.type(screen.getByLabelText('Value'), '0.5');

		expect(form.number).toBe('0.5');
	});
});
