import { render, screen, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import PhoneBar from './PhoneBar.svelte';
import PhoneTabs from './PhoneTabs.svelte';

// A phone's navigation (spec 006 #20): the four screens a page at night opens
// are tabs under the page, and *More* holds the rest in a sheet. *More* keeps
// its name and is lit while one of its screens is on show; the sheet lights
// the screen.

const PROJECT = vi.hoisted(() => 'a'.repeat(32));
const url = { current: new URL(`http://tracepad.test/p/${PROJECT}/runs/abc`) };

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return url.current;
		},
		get params() {
			return { project: PROJECT };
		}
	}
}));
vi.mock('$lib/api/client.svelte', () => ({ api: { version: '0.0.0-test', listProjects: vi.fn() } }));
vi.mock('$lib/auth.svelte', () => ({
	auth: {
		displayName: 'ada@example.com',
		account: { email: 'ada@example.com' },
		owner: false,
		projects: [{ id: PROJECT, name: 'demo', role: 'editor' }]
	},
	LOGIN_ROUTE: '/login'
}));
vi.mock('$lib/session', () => ({ end: vi.fn() }));

// Reduced motion, so the sheet opens and closes without a transition: jsdom
// has no animation to wait for, and the animated path runs end to end.
beforeEach(() => {
	window.matchMedia = (query: string) =>
		({ matches: true, media: query, addEventListener() {}, removeEventListener() {} }) as never;
});

describe('on a phone', () => {
	// The body lock a closed sheet leaves behind for 24ms is jsdom's to
	// measure, not the reader's (see `tests/setup.ts`).
	const person = () => userEvent.setup({ pointerEventsCheck: 0 });
	const tabs = () =>
		within(screen.getByRole('navigation', { name: 'Sections' }))
			.getAllByRole('link')
			.map((link) => link.textContent?.trim());

	it('has four tabs and More, and nothing else until More opens', () => {
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/traces`);
		render(PhoneTabs);

		expect(tabs()).toEqual(['Dashboard', 'Traces', 'Sessions', 'Users']);
		expect(screen.getByRole('link', { name: 'Traces' })).toHaveAttribute('aria-current', 'page');
		const more = screen.getByRole('button', { name: 'More' });
		expect(more).not.toHaveAttribute('aria-current');
		expect(screen.queryByRole('link', { name: 'Prompts' })).not.toBeInTheDocument();
	});

	it('lights More, still named More, on a screen it holds, and the screen in the sheet', async () => {
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/runs/abc`);
		render(PhoneTabs);

		const more = screen.getByRole('button', { name: 'More' });
		expect(more).toHaveAttribute('aria-current', 'true');
		expect(more).toHaveTextContent('More');
		expect(screen.queryAllByRole('link', { current: 'page' })).toHaveLength(0);

		await person().click(more);
		const sheet = screen.getByRole('navigation', { name: 'More sections' });
		const names = within(sheet)
			.getAllByRole('link')
			.map((link) => link.textContent?.trim());
		expect(names).toEqual([
			'Prompts',
			'Datasets',
			'Runs',
			'Score configs',
			'Queues',
			'Quality',
			'Settings'
		]);
		expect(within(sheet).getByText('Evals')).toBeInTheDocument();
		const runs = within(sheet).getByRole('link', { name: 'Runs' });
		expect(runs).toHaveAttribute('aria-current', 'page');
		expect(runs).toHaveAttribute('href', `/p/${PROJECT}/runs`);
	});

	it('closes the sheet on Escape and hands focus back to More', async () => {
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/traces`);
		render(PhoneTabs);

		const more = screen.getByRole('button', { name: 'More' });
		await person().click(more);
		expect(screen.getByRole('dialog', { name: 'More' })).toBeInTheDocument();
		await person().keyboard('{Escape}');

		expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
		expect(more).toHaveFocus();
	});

	it('closes the sheet on the tap of a link, not when the screen arrives', async () => {
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/traces`);
		render(PhoneTabs);

		await person().click(screen.getByRole('button', { name: 'More' }));
		const sheet = screen.getByRole('navigation', { name: 'More sections' });
		// jsdom follows no link, so the URL stays where it was: what closes the
		// sheet here is the tap itself.
		await person().click(within(sheet).getByRole('link', { name: 'Queues' }));

		expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
	});
});

describe("a phone's bar on top", () => {
	it('names the product, the project and the account, and navigates nowhere', () => {
		render(PhoneBar);

		expect(screen.getByText('Tracepad')).toBeInTheDocument();
		expect(screen.getByRole('button', { name: 'Switch project' })).toHaveTextContent('demo');
		expect(screen.getByRole('button', { name: 'Signed in as ada@example.com' })).toBeInTheDocument();
		expect(screen.queryByRole('link')).not.toBeInTheDocument();
	});
});
