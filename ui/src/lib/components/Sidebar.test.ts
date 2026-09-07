import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import Sidebar from './Sidebar.svelte';

// The sidebar's first section (spec 016 #1): a labelled group whose label is
// not a link, whose children are, and whose active state belongs to the child
// the URL is under — `aria-current="page"` keeps meaning what it means.

const url = { current: new URL('http://tracepad.test/runs/abc') };

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return url.current;
		}
	}
}));
vi.mock('$lib/api/client.svelte', () => ({ api: { version: '0.0.0-test' } }));
vi.mock('$lib/auth.svelte', () => ({ auth: { clear: vi.fn() }, LOGIN_ROUTE: '/login' }));
vi.mock('$lib/admin.svelte', () => ({ admin: { clear: vi.fn() } }));
vi.mock('$lib/project.svelte', () => ({ project: { name: 'demo', forget: vi.fn() } }));

describe('the Evals section', () => {
	it('renders the group with its three children as links and the label as text', () => {
		render(Sidebar);
		const nav = screen.getByRole('navigation', { name: 'Sections' });

		expect(screen.getByText('Evals')).toBeInTheDocument();
		expect(screen.queryByRole('link', { name: 'Evals' })).toBeNull();
		for (const [name, href] of [
			['Datasets', '/datasets'],
			['Runs', '/runs'],
			['Score configs', '/score-configs']
		]) {
			expect(screen.getByRole('link', { name })).toHaveAttribute('href', href);
		}
		// Eight destinations in all: the four that were there, the three the
		// section holds, and Prompts beside them (spec 021 #1).
		expect(nav.querySelectorAll('a')).toHaveLength(8);
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
		url.current = new URL('http://tracepad.test/score-configs');
		render(Sidebar);

		const current = screen
			.getAllByRole('link')
			.filter((link) => link.getAttribute('aria-current') === 'page')
			.map((link) => link.textContent?.trim());
		expect(current).toEqual(['Score configs']);
	});
});

// Spec 021 #1: a prompt is a production artefact — what the application ships
// — not an eval noun, so it is a top-level item between Stats and the section
// rather than a fourth child of it, which is the mistake the decision rules
// out.
describe('the Prompts item', () => {
	it('sits at the top level, between Stats and Evals', () => {
		render(Sidebar);
		const nav = screen.getByRole('navigation', { name: 'Sections' });

		expect(screen.getByRole('link', { name: 'Prompts' })).toHaveAttribute('href', '/prompts');
		const order = [...nav.querySelectorAll('a')].map((link) => link.textContent?.trim());
		expect(order.slice(0, 4)).toEqual(['Traces', 'Sessions', 'Stats', 'Prompts']);
		// And the group still holds the three eval screens and no more.
		const group = nav.querySelector('li > ul');
		expect([...(group?.querySelectorAll('a') ?? [])].map((link) => link.textContent?.trim())).toEqual(
			['Datasets', 'Runs', 'Score configs']
		);
	});

	it('is the active one on a prompt page', () => {
		url.current = new URL('http://tracepad.test/prompts/support-answer?version=2');
		render(Sidebar);

		const current = screen
			.getAllByRole('link')
			.filter((link) => link.getAttribute('aria-current') === 'page')
			.map((link) => link.textContent?.trim());
		expect(current).toEqual(['Prompts']);
	});
});
