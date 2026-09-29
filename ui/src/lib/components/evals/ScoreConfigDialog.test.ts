import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import type { ScoreConfig } from '$lib/api/client.svelte';
import ScoreConfigDialog from './ScoreConfigDialog.svelte';

// Where the focus lands when the form opens (spec 006 #26): in the first field
// that takes text, which on an edit is not the read-only name.

vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: { putScoreConfig: vi.fn() }
}));

const config: ScoreConfig = {
	name: 'accuracy',
	data_type: 'numeric',
	direction: 'higher',
	min: 0,
	max: 1,
	categories: null,
	description: null,
	created_at: '2026-09-07T10:00:00Z',
	updated_at: '2026-09-07T10:00:00Z'
};

function open(edited: ScoreConfig | null) {
	render(ScoreConfigDialog, {
		props: { open: true, config: edited, onclose: vi.fn(), onsaved: vi.fn() }
	} as never);
}

describe('where the focus lands', () => {
	it('on the name of a new config', async () => {
		open(null);
		await waitFor(() => expect(screen.getByLabelText('Name')).toHaveFocus());
	});

	it('past the read-only name of an edited one', async () => {
		open(config);
		const name = screen.getByLabelText('Name');
		expect(name).toHaveAttribute('readonly');
		await waitFor(() => expect(screen.getByLabelText('Type')).toHaveFocus());
		expect(name).not.toHaveFocus();
	});
});
