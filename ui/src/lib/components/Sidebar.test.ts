import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
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
