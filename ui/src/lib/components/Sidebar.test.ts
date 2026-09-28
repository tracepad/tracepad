import { render, screen, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Sidebar from './Sidebar.svelte';

// The sidebar's first section (spec 016 #1): a labelled group whose label is
// not a link, whose children are, and whose active state belongs to the child
// the URL is under — `aria-current="page"` keeps meaning what it means. Its
// fourth child is Queues (spec 024 #10). Every link is under the project on
// screen, and the active one is read after that prefix (spec 029 #2).

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

// The desktop column unless a test asks for a phone: the one media query
// answers for the width and for reduced motion alike, so a phone's sheet
// also opens without a transition.
let narrow = false;
beforeEach(() => {
	narrow = false;
	window.matchMedia = (query: string) =>
		({ matches: narrow, media: query, addEventListener() {}, removeEventListener() {} }) as never;
});

describe('the Evals section', () => {
	it('renders the group with its five children as links and the label as text', () => {
		render(Sidebar);
		const nav = screen.getByRole('navigation', { name: 'Sections' });

		expect(screen.getByText('Evals')).toBeInTheDocument();
		expect(screen.queryByRole('link', { name: 'Evals' })).toBeNull();
		for (const [name, href] of [
			['Datasets', '/datasets'],
			['Runs', '/runs'],
			['Score configs', '/score-configs'],
			['Queues', '/queues'],
			['Quality', '/quality']
		]) {
			expect(screen.getByRole('link', { name })).toHaveAttribute('href', `/p/${PROJECT}${href}`);
		}
		// Eleven destinations in all: the four that were there — Stats now
		// the Dashboard (spec 034 #1) — the five the section holds, Prompts
		// beside them (spec 021 #1) and Users (spec 023 #8).
		expect(nav.querySelectorAll('a')).toHaveLength(11);
	});

	it('marks the active child and nothing else', () => {
		render(Sidebar);

		expect(screen.getByRole('link', { name: 'Runs' })).toHaveAttribute('aria-current', 'page');
		expect(screen.getByRole('link', { name: 'Datasets' })).not.toHaveAttribute('aria-current');
		expect(screen.getByRole('link', { name: 'Traces' })).not.toHaveAttribute('aria-current');
	});

	// Four destinations share an initial: a prefix looser than the whole
	// path would light Sessions, Stats and Settings beside Score configs.
	it('tells the destinations apart by their whole path', () => {
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/score-configs`);
		render(Sidebar);

		const current = screen
			.getAllByRole('link')
			.filter((link) => link.getAttribute('aria-current') === 'page')
			.map((link) => link.textContent?.trim());
		expect(current).toEqual(['Score configs']);
	});
});

// Spec 021 #1: a prompt is a production artefact — what the application ships
// — not an eval noun, so it is a top-level item between Users and the section
// rather than a fourth child of it, which is the mistake the decision rules
// out.
describe('the Prompts item', () => {
	it('sits at the top level, between Users and Evals', () => {
		render(Sidebar);
		const nav = screen.getByRole('navigation', { name: 'Sections' });

		expect(screen.getByRole('link', { name: 'Prompts' })).toHaveAttribute(
			'href',
			`/p/${PROJECT}/prompts`
		);
		const order = [...nav.querySelectorAll('a')].map((link) => link.textContent?.trim());
		// The dashboard is the door and comes first (spec 034 #1); Stats is
		// gone from the sections, since it is the dashboard. Users sits
		// between Sessions and Prompts, the screens it joins (spec 023 #8).
		expect(order.slice(0, 5)).toEqual(['Dashboard', 'Traces', 'Sessions', 'Users', 'Prompts']);
		expect(order).not.toContain('Stats');
		expect(screen.getByRole('link', { name: 'Dashboard' })).toHaveAttribute(
			'href',
			`/p/${PROJECT}/dashboard`
		);
		// And the group holds the eval screens and no more.
		const group = nav.querySelector('li > ul');
		expect([...(group?.querySelectorAll('a') ?? [])].map((link) => link.textContent?.trim())).toEqual(
			['Datasets', 'Runs', 'Score configs', 'Queues', 'Quality']
		);
	});

	it('is the active one on a prompt page', () => {
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/prompts/support-answer?version=2`);
		render(Sidebar);

		const current = screen
			.getAllByRole('link')
			.filter((link) => link.getAttribute('aria-current') === 'page')
			.map((link) => link.textContent?.trim());
		expect(current).toEqual(['Prompts']);
	});
});

// The switcher stands where the name did (spec 029 #5); its own behaviour
// has its own test.
describe('the project', () => {
	it('is named by the switcher', () => {
		render(Sidebar);

		expect(screen.getByRole('button', { name: 'Switch project' })).toHaveTextContent('demo');
	});
});

// Spec 006 #20: on a phone the four screens a page at night opens are tabs
// under the thumb, and *More* holds the rest in a sheet. *More* keeps its name
// and is lit while one of its screens is on show; the sheet lights the screen.
describe('on a phone', () => {
	// The body lock a closed sheet leaves behind for 24ms is jsdom's to
	// measure, not the reader's (see `tests/setup.ts`).
	const person = () => userEvent.setup({ pointerEventsCheck: 0 });
	const tabs = () =>
		within(screen.getByRole('navigation', { name: 'Sections' }))
			.getAllByRole('link')
			.map((link) => link.textContent?.trim());

	it('has four tabs and More, and nothing else until More opens', () => {
		narrow = true;
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/traces`);
		render(Sidebar);

		expect(tabs()).toEqual(['Dashboard', 'Traces', 'Sessions', 'Users']);
		expect(screen.getByRole('link', { name: 'Traces' })).toHaveAttribute('aria-current', 'page');
		const more = screen.getByRole('button', { name: 'More' });
		expect(more).not.toHaveAttribute('aria-current');
		expect(screen.queryByRole('link', { name: 'Prompts' })).not.toBeInTheDocument();
		expect(screen.getByRole('button', { name: 'Switch project' })).toHaveTextContent('demo');
	});

	it('lights More, still named More, on a screen it holds, and the screen in the sheet', async () => {
		narrow = true;
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/runs/abc`);
		render(Sidebar);

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
		narrow = true;
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/traces`);
		render(Sidebar);

		const more = screen.getByRole('button', { name: 'More' });
		await person().click(more);
		expect(screen.getByRole('dialog', { name: 'More' })).toBeInTheDocument();
		await person().keyboard('{Escape}');

		expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
		expect(more).toHaveFocus();
	});
});
