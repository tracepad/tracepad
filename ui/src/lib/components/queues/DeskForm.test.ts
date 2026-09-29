import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AnnotationItem, Score, ScoreConfig } from '$lib/api/client.svelte';
import DeskForm from './DeskForm.svelte';

// The desk's form (spec 024 #12): one control per score the queue asks for,
// prefilled from what is already on the target, and a *Complete* that is not
// offered until the shape the server checks is filled.

const config = (name: string, extra: Partial<ScoreConfig> = {}): ScoreConfig => ({
	name,
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

const configs = [
	config('accuracy'),
	config('tone', { data_type: 'categorical', categories: ['warm', 'curt'], min: null, max: null })
];

const item: AnnotationItem = {
	id: 'i'.repeat(32),
	trace_id: 't'.repeat(32),
	status: 'pending',
	seq: 1,
	added_at: '2026-09-07T10:00:00Z'
};

const onsave = vi.fn();
const onskip = vi.fn();

function desk(scores: Score[] = [], missing: string[] = [], names = ['accuracy', 'tone']) {
	render(DeskForm, {
		props: {
			queue: 'weekly',
			item,
			configs,
			names,
			scores,
			missing,
			onsave,
			onskip,
			onlater: vi.fn()
		}
	} as never);
	return userEvent.setup({ delay: null });
}

const complete = () => screen.getByRole('button', { name: /Complete/ });

beforeEach(() => {
	onsave.mockClear();
	onskip.mockClear();
});

describe('the completeness rule, client-side', () => {
	it('refuses Complete until every score the queue asks for is set', async () => {
		const user = desk();

		expect(complete()).toBeDisabled();
		expect(screen.getByText(/Still to set: accuracy, tone/)).toBeVisible();

		await user.type(screen.getByLabelText('accuracy'), '0.9');
		expect(complete()).toBeDisabled();
		expect(screen.getByText(/Still to set: tone/)).toBeVisible();

		await user.selectOptions(screen.getByLabelText('tone'), 'warm');
		expect(complete()).toBeEnabled();
	});

	// The whole point of prefilling (#7, #12): a verdict already on the target
	// counts, whoever wrote it, so the reviewer confirms rather than repeats.
	it('opens complete when the target is already scored, and posts nothing', async () => {
		const user = desk([
			{
				id: 'a'.repeat(32),
				trace_id: item.trace_id,
				author: null,
				name: 'accuracy',
				data_type: 'numeric',
				value: 0.5,
				timestamp: '2026-09-07T10:00:00Z',
				created_at: '2026-09-07T10:00:00Z'
			},
			{
				id: 'b'.repeat(32),
				trace_id: item.trace_id,
				author: null,
				name: 'tone',
				data_type: 'categorical',
				string_value: 'warm',
				timestamp: '2026-09-07T10:00:00Z',
				created_at: '2026-09-07T10:00:00Z'
			}
		]);

		expect(screen.getAllByText('already scored')).toHaveLength(2);
		expect(complete()).toBeEnabled();

		await user.click(complete());
		expect(onsave).toHaveBeenCalledWith([]);
	});

	it('posts only what the reviewer changed', async () => {
		const user = desk([
			{
				id: 'a'.repeat(32),
				trace_id: item.trace_id,
				author: null,
				name: 'accuracy',
				data_type: 'numeric',
				value: 0.5,
				timestamp: '2026-09-07T10:00:00Z',
				created_at: '2026-09-07T10:00:00Z'
			}
		]);
		await user.selectOptions(screen.getByLabelText('tone'), 'curt');
		await user.click(complete());

		const posted = onsave.mock.calls[0][0] as { name: string; metadata: unknown }[];
		expect(posted.map((body) => body.name)).toEqual(['tone']);
		expect(posted[0].metadata).toEqual({
			source: 'annotation',
			queue: 'weekly'
		});
	});
});

// The server is the oracle (#7): when the two disagree — a score deleted from
// another tab, say — the `409` marks the control it named.
it('marks the controls the server said were missing', () => {
	desk([], ['tone']);

	// Twice: at the control, and once by the buttons where the sentence is.
	expect(screen.getAllByRole('alert')).toHaveLength(2);
	expect(screen.getByText('the server has no score for this')).toBeVisible();
	expect(screen.getByText(/found no score for tone/)).toBeVisible();
});

// The edge case: the config was deleted and the queue kept asking for the name.
it('keeps a name whose config is gone, and says nothing bounds it', () => {
	desk([], [], ['vibes']);

	expect(screen.getByLabelText('vibes')).toBeVisible();
	expect(screen.getByText(/no score config any more/)).toBeVisible();
});

it('asks for a reason before it skips', async () => {
	const user = desk();
	await user.click(screen.getByRole('button', { name: 'Skip…' }));
	await user.type(screen.getByLabelText(/Why skip/), 'nothing to judge');
	await user.click(screen.getByRole('button', { name: 'Skip it' }));

	expect(onskip).toHaveBeenCalledWith('nothing to judge');
});
