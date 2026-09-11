import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import ProjectSwitcher from './ProjectSwitcher.svelte';

// The switcher (spec 029 #5): the projects the account can reach, by name,
// the current one marked, each with its traces of the last day, a filter box
// once the list is long, and *New project* for an owner (#7). Choosing one
// lands on the same section of the other project (#6).

const goto = vi.fn();
const listProjects = vi.fn();
const params = { current: {} as { project?: string } };
const url = { current: new URL('http://tracepad.test/') };
const session = {
	owner: false,
	projects: [] as { id: string; name: string; role: 'viewer' | 'editor' | 'owner' }[]
};

vi.mock('$app/navigation', () => ({ goto: (...args: unknown[]) => goto(...args) }));
vi.mock('$app/state', () => ({
	page: {
		get params() {
			return params.current;
		},
		get url() {
			return url.current;
		}
	}
}));
vi.mock('$lib/api/client.svelte', () => ({
	api: { listProjects: (...args: unknown[]) => listProjects(...args) }
}));
vi.mock('$lib/auth.svelte', () => ({
	get auth() {
		return session;
	}
}));
vi.mock('$lib/session', () => ({ refresh: vi.fn() }));

const id = (n: number) => String(n).repeat(32).slice(0, 32);
const CHECKOUT = { id: id(1), name: 'checkout', role: 'editor' as const };
const STAGING = { id: id(2), name: 'staging', role: 'viewer' as const };

function at(path: string, project = CHECKOUT.id) {
	params.current = { project };
	url.current = new URL(`http://tracepad.test/p/${project}${path}`);
}

beforeEach(() => {
	vi.unstubAllGlobals();
	goto.mockClear();
	listProjects.mockReset();
	listProjects.mockResolvedValue({
		projects: [
			{ ...CHECKOUT, traces_24h: 1234 },
			{ ...STAGING, traces_24h: 0 }
		]
	});
	session.owner = false;
	session.projects = [CHECKOUT, STAGING];
	at('/traces');
});

/**
 * Opens the menu. `hidden` and `pointerEventsCheck` are about jsdom rather than
 * the menu (see `AccountMenu.test.ts`): the floating wrapper stays hidden
 * where there is no layout engine, and the Playwright suite drives the real
 * thing.
 */
async function open() {
	const person = userEvent.setup({ pointerEventsCheck: 0 });
	render(ProjectSwitcher);
	await person.click(screen.getByRole('button', { name: 'Switch project' }));
	const rows = await screen.findAllByRole('menuitemradio', { hidden: true });
	return { person, rows };
}

const names = (rows: HTMLElement[]) =>
	rows.map((row) => row.querySelector('span > span')?.textContent?.trim());

describe('the trigger', () => {
	it('names the project on screen', () => {
		render(ProjectSwitcher);

		expect(screen.getByRole('button', { name: 'Switch project' })).toHaveTextContent('checkout');
	});

	it('says so when there is none', () => {
		params.current = { project: id(9) };
		render(ProjectSwitcher);

		expect(screen.getByRole('button', { name: 'Switch project' })).toHaveTextContent(
			'Choose a project'
		);
	});
});

describe('the menu', () => {
	it('lists the projects by name and marks the current one', async () => {
		const { rows } = await open();

		expect(names(rows)).toEqual(['checkout', 'staging']);
		expect(rows[0]).toHaveAttribute('aria-checked', 'true');
		expect(rows[1]).toHaveAttribute('aria-checked', 'false');
	});

	// Asked on every open and never on load (#8): the count is wanted about
	// a project that is not on screen, which is only ever seen from here.
	it('captions each row with its traffic of the last day, once it arrives', async () => {
		const { rows } = await open();

		expect(listProjects).toHaveBeenCalledWith({ activity: '24h' });
		await waitFor(() => expect(rows[0]).toHaveTextContent('1,234 traces · 24h'));
		expect(rows[1]).toHaveTextContent('no traces · 24h');
	});

	it('asks nothing until it is opened', () => {
		render(ProjectSwitcher);

		expect(listProjects).not.toHaveBeenCalled();
	});

	it('goes to the same section of the chosen project', async () => {
		at(`/traces/${'c'.repeat(32)}?environment=prod&peek=x`);
		const { person, rows } = await open();

		await person.click(rows[1]);

		expect(goto).toHaveBeenCalledWith(`/p/${STAGING.id}/traces?environment=prod`);
	});

	// The same one chosen again is nothing happening (edge cases).
	it('does nothing for the project already on screen', async () => {
		const { person, rows } = await open();

		await person.click(rows[0]);

		expect(goto).not.toHaveBeenCalled();
	});

	// Creating a project is an owner's action (spec 028 #3), and the menu
	// hides what the role cannot do (spec 028 #15).
	// Found by text rather than by name: a wrapper jsdom cannot lay out stays
	// hidden, and a hidden element has no accessible name to match.
	const actions = () =>
		screen.queryAllByRole('menuitem', { hidden: true }).map((one) => one.textContent?.trim());

	it('offers New project to an owner', async () => {
		session.owner = true;
		await open();

		expect(actions()).toEqual(['New project']);
	});

	it('does not offer it to anybody else', async () => {
		await open();

		expect(actions()).toEqual([]);
	});
});

describe('the filter box', () => {
	const many = Array.from({ length: 9 }, (_, i) => ({
		id: id(i + 1),
		name: `project-${String.fromCharCode(97 + i)}`,
		role: 'viewer' as const
	}));

	it('is absent while the list fits in a glance', async () => {
		await open();

		expect(screen.queryByLabelText('Filter projects')).toBeNull();
	});

	it('appears over eight projects and narrows the list by substring', async () => {
		session.projects = many;
		at('/traces', many[0].id);
		// The menu moves focus into itself a frame after opening. A browser
		// has done that long before anybody reaches the box; jsdom's frame is
		// a timer that lands mid-typing and takes the second letter with it.
		// A frame that is a zero timer fires in order, so one turn of the
		// clock after opening it has fired.
		vi.stubGlobal('requestAnimationFrame', (frame: FrameRequestCallback) =>
			setTimeout(() => frame(performance.now()), 0)
		);
		const { person } = await open();
		await new Promise((done) => setTimeout(done, 0));

		const box = screen.getByLabelText<HTMLInputElement>('Filter projects');
		await person.type(box, 'T-C');

		expect(box.value).toBe('T-C');
		await waitFor(() =>
			expect(names(screen.getAllByRole('menuitemradio', { hidden: true }))).toEqual(['project-c'])
		);
	});
});
