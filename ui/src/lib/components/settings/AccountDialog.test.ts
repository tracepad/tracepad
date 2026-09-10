import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import AccountDialog from './AccountDialog.svelte';

// Editing an account writes the difference and nothing else (spec 028 #12).
// The membership rows are the part with a shape that hides mistakes: the form
// holds a role for every project on the server, most of which this account is
// not in, and "not in it" and "no access" have to compare equal — otherwise a
// save with nothing changed sends one DELETE per project (found in review of
// PR #54).

const patchAccount = vi.fn(async () => ({ account: {} }));
const putMembership = vi.fn(async () => ({ membership: {} }));
const deleteMembership = vi.fn(async () => undefined);
const createAccount = vi.fn(async () => ({ account: {}, invite_url: '', invite_expires_at: '' }));

vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		patchAccount: (...args: unknown[]) => patchAccount(...(args as [])),
		putMembership: (...args: unknown[]) => putMembership(...(args as [])),
		deleteMembership: (...args: unknown[]) => deleteMembership(...(args as [])),
		createAccount: (...args: unknown[]) => createAccount(...(args as []))
	}
}));

const PROJECTS = [
	{ id: 'p1', name: 'checkout' },
	{ id: 'p2', name: 'staging' },
	{ id: 'p3', name: 'sandbox' }
];

/** An account with a role in exactly one of the three. */
const account = () => ({
	id: 'acc1',
	email: 'helper@example.com',
	name: 'The Helper',
	owner: false,
	disabled: false,
	pending: false,
	created_at: '2026-09-01T00:00:00Z',
	last_login_at: null,
	projects: [{ id: 'p1', name: 'checkout', role: 'viewer' }]
});

function open(target: unknown = account()) {
	const saved = vi.fn(async () => {});
	const closed = vi.fn();
	render(AccountDialog, {
		account: target,
		projects: PROJECTS,
		oninvited: vi.fn(),
		onsaved: saved,
		onclose: closed
	} as never);
	return { person: userEvent.setup({ pointerEventsCheck: 0 }), saved, closed };
}

const save = (person: ReturnType<typeof userEvent.setup>) =>
	person.click(screen.getByRole('button', { name: 'Save' }));

/** Waits until the dialog has finished pulling focus into itself. */
const settled = () =>
	waitFor(() => expect(screen.getByRole('dialog').contains(document.activeElement)).toBe(true));

beforeEach(() => {
	patchAccount.mockClear();
	putMembership.mockClear();
	deleteMembership.mockClear();
	document.body.style.pointerEvents = '';
});

describe('editing an account', () => {
	it('writes no membership at all when none of them changed', async () => {
		const { person, saved } = open();

		await save(person);

		expect(patchAccount).toHaveBeenCalledWith('acc1', {
			name: 'The Helper',
			owner: false,
			disabled: false
		});
		// The two projects this account is not in are not touched, and the one
		// it is in is at the role it already had.
		expect(deleteMembership).not.toHaveBeenCalled();
		expect(putMembership).not.toHaveBeenCalled();
		expect(saved).toHaveBeenCalled();
	});

	it('writes the one row that moved', async () => {
		const { person } = open();

		await person.selectOptions(screen.getByLabelText('Role in staging'), 'editor');
		await save(person);

		expect(putMembership).toHaveBeenCalledTimes(1);
		expect(putMembership).toHaveBeenCalledWith('acc1', 'p2', 'editor');
		expect(deleteMembership).not.toHaveBeenCalled();
	});

	it('drops the one row that was taken away', async () => {
		const { person } = open();

		await person.selectOptions(screen.getByLabelText('Role in checkout'), '');
		await save(person);

		expect(deleteMembership).toHaveBeenCalledTimes(1);
		expect(deleteMembership).toHaveBeenCalledWith('acc1', 'p1');
	});

	// An owner has every project, so the server drops the rows itself
	// (Decision 12) and sending them would be writing what is about to go.
	it('writes no memberships for somebody being made an owner', async () => {
		const { person } = open();

		await person.click(screen.getByRole('checkbox', { name: /^Owner/ }));
		await save(person);

		expect(patchAccount).toHaveBeenCalledWith('acc1', {
			name: 'The Helper',
			owner: true,
			disabled: false
		});
		expect(deleteMembership).not.toHaveBeenCalled();
		expect(putMembership).not.toHaveBeenCalled();
	});
});

describe('inviting somebody', () => {
	it('sends only the projects that were picked', async () => {
		const { person } = open(null);

		// The dialog moves focus into itself when it opens, and it does it in
		// an effect — so typing before that has happened types at whatever the
		// trap is about to take focus from. Waiting for it, and then clicking
		// the field the way a person reaches one, is what makes the keystrokes
		// land: without it the address arrived half-written, and under a
		// parallel run not at all.
		await settled();
		await person.click(screen.getByLabelText('Email'));
		await person.type(screen.getByLabelText('Email'), 'new@example.com');
		await person.selectOptions(screen.getByLabelText('Role in sandbox'), 'viewer');

		// Waited for rather than assumed: the button is disabled until the
		// email field has something in it, so a click sent the instant after
		// the typing lands on a dead control and nothing happens — which is
		// how this test failed once in a parallel run and never alone.
		const invite = screen.getByRole('button', { name: 'Invite' });
		await waitFor(() => expect(invite).toBeEnabled());
		await person.click(invite);

		expect(createAccount).toHaveBeenCalledWith({
			email: 'new@example.com',
			name: '',
			owner: false,
			memberships: [{ project_id: 'p3', role: 'viewer' }]
		});
	});
});
