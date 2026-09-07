import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import LabelControl from './LabelControl.svelte';

// The label control's one rule (spec 021 #6): a label that points at another
// version is a deploy and asks first, naming the move; a label that points
// nowhere is a note and lands straight away. A removal asks too — what is
// fetching the prompt by that label stops resolving.

const putPromptLabel = vi.fn(async () => ({ label: 'production', version: 7 }));
const deletePromptLabel = vi.fn(async () => ({ label: 'production', version: 7 }));

vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		putPromptLabel: (...args: unknown[]) => putPromptLabel(...(args as [])),
		deletePromptLabel: (...args: unknown[]) => deletePromptLabel(...(args as []))
	}
}));

const onchanged = vi.fn();

function control(props: {
	labels?: string[];
	named?: Record<string, number>;
	version?: number;
} = {}) {
	render(LabelControl, {
		name: 'support',
		version: props.version ?? 7,
		labels: props.labels ?? [],
		named: props.named ?? {},
		onchanged
	} as never);
}

async function type(label: string) {
	await userEvent.click(screen.getByRole('button', { name: /Add label/ }));
	await userEvent.type(screen.getByLabelText('Label to add'), label);
	await userEvent.click(screen.getByRole('button', { name: 'Add' }));
}

const dialog = () => screen.queryByRole('alertdialog');

beforeEach(() => {
	putPromptLabel.mockClear();
	deletePromptLabel.mockClear();
	onchanged.mockClear();
	// A dialog that was open when the last test ended leaves the body inert;
	// the next test's render is a fresh document as far as it is concerned.
	document.body.style.pointerEvents = '';
});

describe('attaching a label', () => {
	it('is immediate when the label points nowhere', async () => {
		control({ named: { staging: 3 } });
		await type('canary');

		expect(dialog()).toBeNull();
		expect(putPromptLabel).toHaveBeenCalledWith('support', 'canary', 7);
		expect(onchanged).toHaveBeenCalled();
	});

	it('asks first when the label is on another version, and names the move', async () => {
		control({ named: { production: 6 } });
		await type('production');

		expect(putPromptLabel).not.toHaveBeenCalled();
		expect(dialog()).not.toBeNull();
		// The sentence a rollback reads before confirming.
		expect(screen.getByText(/production: v6 → v7/)).toBeInTheDocument();

		await userEvent.click(screen.getByRole('button', { name: 'Move production here' }));
		expect(putPromptLabel).toHaveBeenCalledWith('support', 'production', 7);
		expect(onchanged).toHaveBeenCalled();
	});

	it('does nothing when the label is already on this version', async () => {
		control({ labels: ['production'], named: { production: 7 } });
		await type('production');

		expect(dialog()).toBeNull();
		expect(putPromptLabel).not.toHaveBeenCalled();
	});
});

describe('removing a label', () => {
	it('asks first and says what stops resolving', async () => {
		control({ labels: ['production'], named: { production: 7 } });
		await userEvent.click(screen.getByRole('button', { name: 'Remove production' }));

		expect(deletePromptLabel).not.toHaveBeenCalled();
		expect(screen.getByText(/Remove production from v7/)).toBeInTheDocument();

		// The dialog's own button, not the chip's ×, which shares its name.
		const dialogue = dialog();
		expect(dialogue).not.toBeNull();
		await userEvent.click(
			screen.getAllByRole('button', { name: 'Remove production' }).at(-1) as HTMLElement
		);
		expect(deletePromptLabel).toHaveBeenCalledWith('support', 'production');
		expect(onchanged).toHaveBeenCalled();
	});
});
