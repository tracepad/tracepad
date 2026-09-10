import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Page from './+page.svelte';

// The login form (spec 028 #13). Every way of failing to sign in — wrong
// email, wrong password, disabled, invited and never accepted — is one 401
// with one sentence (Decision 8), and this screen's whole job on failure is to
// print that sentence rather than to guess which of them it was.

// Hoisted with the mock factories that use it: `vi.mock` runs before the
// module body, so a plain declaration here would not exist yet.
const { ApiError } = vi.hoisted(() => ({ ApiError: class extends Error {} }));

const url = { current: new URL('http://tracepad.test/login') };
const goto = vi.fn();
const login = vi.fn();
const begin = vi.fn();

vi.mock('$app/navigation', () => ({ goto: (...args: unknown[]) => goto(...args) }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return url.current;
		}
	}
}));
vi.mock('$lib/api/client.svelte', () => ({
	ApiError,
	api: { login: (...args: unknown[]) => login(...args) }
}));
vi.mock('$lib/session', () => ({ begin: () => begin() }));

async function signIn(email = 'her@example.com', password = 'a-long-password') {
	const person = userEvent.setup();
	await person.type(screen.getByLabelText('Email'), email);
	await person.type(screen.getByLabelText('Password'), password);
	await person.click(screen.getByRole('button', { name: 'Sign in' }));
}

beforeEach(() => {
	url.current = new URL('http://tracepad.test/login');
	goto.mockClear();
	begin.mockClear();
	login.mockReset();
	login.mockResolvedValue({ account: {} });
});

describe('signing in', () => {
	it('reads the session and goes where the guard interrupted', async () => {
		url.current = new URL('http://tracepad.test/login?next=%2Ftraces%3Fstatus%3Derror');
		render(Page);

		await signIn();

		await waitFor(() => expect(login).toHaveBeenCalledWith('her@example.com', 'a-long-password'));
		expect(begin).toHaveBeenCalled();
		expect(goto).toHaveBeenCalledWith('/traces?status=error', { replaceState: true });
	});

	it('cannot be submitted empty', () => {
		render(Page);

		expect(screen.getByRole('button', { name: 'Sign in' })).toBeDisabled();
	});
});

describe('when the server refuses', () => {
	it("renders the server's own sentence and stays put", async () => {
		login.mockRejectedValue(new ApiError('wrong email or password'));
		render(Page);

		await signIn();

		const alert = await screen.findByRole('alert');
		expect(alert).toHaveTextContent('wrong email or password');
		expect(goto).not.toHaveBeenCalled();
	});

	// A rate-limited attempt is a different sentence about the same form, and
	// the screen has no opinion about which one it got.
	it('renders a refusal it has never seen before', async () => {
		login.mockRejectedValue(new ApiError('too many attempts; try again in 15 minutes'));
		render(Page);

		await signIn();

		expect(await screen.findByRole('alert')).toHaveTextContent('try again in 15 minutes');
	});

	it('says something of its own only when the failure carries no words', async () => {
		login.mockRejectedValue(new TypeError('Failed to fetch'));
		render(Page);

		await signIn();

		expect(await screen.findByRole('alert')).toHaveTextContent('Something went wrong signing in.');
	});
});
