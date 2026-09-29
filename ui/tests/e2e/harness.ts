import { expect, type Locator, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

/** The port the suite boots the binary on. */
export const PORT = Number(process.env.TRACEPAD_E2E_PORT ?? 47318);

/** Whether this run is the merge gate rather than someone's laptop. */
export const CI = !!process.env.CI;

/** Where the boot writes what the tests need to know about it. */
export const STATE = join(process.cwd(), 'tests', 'e2e', '.state.json');

/** An account the suite can sign in as. */
export type Account = { email: string; password: string };

export type State = {
	baseURL: string;
	/** The key of the project first run created, which the corpus is in. */
	key: string;
	/** That project's id, which is what the seeded accounts can reach. */
	project: string;
	/** The first owner, created from the setup link the server printed. */
	owner: Account & { id: string };
	/** An editor of the seeded project, for the suites that only read it. */
	member: Account;
};

/**
 * The admin token the suite boots the server with. It is a fixture, not a
 * secret: this server lives for the length of one test run on a temporary
 * database. No screen takes it any more (spec 028 #14) — it is how this file
 * mints projects and accounts out of band, the way `tracepad accounts` does.
 */
export const ADMIN_TOKEN = 'e2e-admin-token-00000000000000000000';

/** One password for every fixture account; ten characters is the rule (#1). */
export const PASSWORD = 'e2e-password';

/**
 * Mints a project of its own, so a test that changes retention, revokes a key
 * or deletes something is not doing it to the project another test is reading.
 * The two Playwright projects run the same files against one server.
 *
 * It comes with an editor account of its own for the same reason, and for one
 * more: a bare path redirects to the remembered project, which for an account
 * that has opened nothing yet is the first of `me.projects` by name (spec 029
 * #3) — so an account that can reach exactly one project is an account whose
 * bare `page.goto('/traces')` lands on the project this suite made.
 */
export async function createProject(
	name: string
): Promise<{ id: string; name: string; key: string; account: Account }> {
	const { baseURL } = state();
	const unique = `${name}-${Math.random().toString(36).slice(2, 8)}`;
	const response = await fetch(`${baseURL}/api/v1/projects`, {
		method: 'POST',
		headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, 'Content-Type': 'application/json' },
		body: JSON.stringify({ name: unique })
	});
	if (!response.ok) throw new Error(`create project: ${response.status}`);
	const created = (await response.json()) as { id: string; name: string; secret_key: string };
	const account = await inviteEditor(baseURL, created.id, unique);
	return { id: created.id, name: created.name, key: created.secret_key, account };
}

/**
 * Invites an editor of one project and accepts the invitation, which is the
 * only way an account gets a password (spec 028 #10). Two requests, both of
 * them the ones an owner and an invited person make.
 */
export async function inviteEditor(
	baseURL: string,
	project: string,
	label: string
): Promise<Account> {
	return invite(baseURL, `${label}@e2e.test`, [{ project_id: project, role: 'editor' }]);
}

/** A viewer of one project, for the suites about what a viewer is not offered. */
export async function inviteViewer(
	baseURL: string,
	project: string,
	label: string
): Promise<Account> {
	return invite(baseURL, `${label}-viewer@e2e.test`, [{ project_id: project, role: 'viewer' }]);
}

/**
 * Invites an account that is a member of nothing — the one whose every bare
 * path is `/p` (spec 029 #4) and whose Account tab is still its own (#14).
 */
export async function inviteNobody(baseURL: string, label: string): Promise<Account> {
	return invite(baseURL, `${label}-${Math.random().toString(36).slice(2, 8)}@e2e.test`, []);
}

async function invite(
	baseURL: string,
	email: string,
	memberships: { project_id: string; role: 'editor' | 'viewer' }[]
): Promise<Account> {
	const response = await fetch(`${baseURL}/api/v1/accounts`, {
		method: 'POST',
		headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, 'Content-Type': 'application/json' },
		body: JSON.stringify({ email, memberships })
	});
	if (!response.ok) throw new Error(`invite ${email}: ${response.status} ${await response.text()}`);
	const { invite_url } = (await response.json()) as { invite_url: string };
	await acceptInvite(baseURL, invite_url);
	return { email, password: PASSWORD };
}

/** Sets a password from an invitation link, the way the `/invite` screen does. */
export async function acceptInvite(baseURL: string, link: string) {
	const token = new URLSearchParams(new URL(link).hash.slice(1)).get('token');
	if (!token) throw new Error(`no #token= in the invitation link: ${link}`);
	const response = await fetch(`${baseURL}/api/v1/auth/accept-invite`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json', 'X-Forwarded-For': ownAddress() },
		body: JSON.stringify({ token, password: PASSWORD })
	});
	if (!response.ok) throw new Error(`accept the invitation: ${response.status}`);
}

/**
 * An address of its own, for a page or a request that checks a password.
 * The server counts password checks per source (spec 046) and reads the
 * source from `X-Forwarded-For` when the peer is a loopback proxy, which it
 * trusts by default. Every test connects from 127.0.0.1, so without this the
 * whole suite would be one source, and its hundreds of sign-ins would meet
 * the limit a flood from one address meets. With it each sign-in arrives from
 * an address of its own, as the people of a real deployment do. The range is
 * 198.18.0.0/15, set aside for testing (RFC 2544); two tests that draw the
 * same one share twenty checks, which is more than either makes.
 */
export function ownAddress(): string {
	const n = Math.floor(Math.random() * 2 ** 17);
	return `198.${18 + (n >> 16)}.${(n >> 8) & 255}.${n & 255}`;
}

/** Sends every request of the page from an address of its own (ownAddress). */
export async function fromOwnAddress(page: Page) {
	await page.setExtraHTTPHeaders({ 'X-Forwarded-For': ownAddress() });
}

/**
 * How long to wait for what a click that runs bcrypt shows: a sign-in, a
 * password set or changed. Twenty seconds, not the default five: bcrypt at
 * the production cost is a quarter of a second on an idle machine and
 * several on one busy with parallel workers and browsers. The binary under
 * test is the one that ships, so the wait moves rather than the cost.
 */
export const BCRYPT_WAIT = 20_000;

/**
 * Signs in through the form, which is the one way in (spec 028 #13). The
 * cookie the server sets is the session for the rest of the test, and the
 * form lands on the remembered project's dashboard (spec 029 #3, spec 034
 * #1), which writes its window into the address.
 *
 * The suites navigate by bare path — `page.goto('/traces?…')` — and go
 * through that same redirect, the way every link written before the prefix
 * does; the one suite about the prefix itself asserts the `/p/{id}` shapes.
 */
export async function signIn(page: Page, account: Account) {
	await fromOwnAddress(page);
	await page.goto('/login');
	await page.getByLabel('Email').fill(account.email);
	// Exact: the eye beside the field is labelled "Show the password".
	await page.getByLabel('Password', { exact: true }).fill(account.password);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).toHaveURL(/\/p\/[0-9a-f]{32}\/dashboard(\?|$)/, { timeout: BCRYPT_WAIT });
}

/**
 * Signs in as the owner with one project pinned. An owner reaches every
 * project, so "the first by name" is whichever project some other worker
 * happened to create; the remembered project lives in `localStorage` under
 * the account's id (spec 029 #3), and this is how a test states it.
 */
export async function signInAsOwner(page: Page, project?: string) {
	const { owner } = state();
	if (project) {
		await page.addInitScript(
			([id, chosen]: [string, string]) =>
				window.localStorage.setItem(`tracepad.project.${id}`, chosen),
			[owner.id, project] as [string, string]
		);
	}
	await signIn(page, owner);
}

export function state(): State {
	return JSON.parse(readFileSync(STATE, 'utf8')) as State;
}

/** The fixture whose payloads are large enough to meet a response budget. */
export const LARGE_PAYLOAD_TRACE = '0071122334455667788990aabbccddee';
/** Its generation — the observation that carries the truncated input. */
export const LARGE_PAYLOAD_OBSERVATION = '1112131415161719';
/** The fixture with two failing observations under a healthy root. */
export const FAILING_TRACE = 'dd44ee55ff6677008899001122aabb33';

/**
 * The fixture whose payloads are not JSON: a bare multi-line prompt and its
 * answer, the shape spec 015 #12 shows as text rather than as a document.
 */
export const PLAIN_TEXT_TRACE = 'bc0de1f2a3b4c5d6e7f80910a1b2c3d4';
/** Its only observation, the generation carrying both. */
export const PLAIN_TEXT_OBSERVATION = 'b0b1b2b3b4b5b6b7';

/**
 * What that observation's payloads say, read from the golden the mapper writes
 * rather than copied here. They are a Go string literal (`internal/otlptest`,
 * `plainTextPrompt`), and a copy of one would go stale silently: rewording the
 * prompt regenerates the body and its golden through `make fixtures` and says
 * nothing about a constant in this file, so only the separate e2e job would
 * notice, pointing at a line nobody touched.
 */
export function plainTextPayloads(): { input: string; output: string } {
	const golden = join(
		resolve(process.cwd(), '..'),
		'testdata',
		'golden',
		'011-plain-text-prompt.json'
	);
	const mapped = JSON.parse(readFileSync(golden, 'utf8')) as {
		observations: { input: string; output: string }[];
	};
	return mapped.observations[0];
}

/**
 * The fixture that carries what the wire already sends (spec 012): a release,
 * a tool call, and a generation with a completion start and a prompt link.
 */
export const WIRE_TRACE = 'ff6677008899001122aabb33cc44dd55';
/** Its generation — prompt `support-answer@7`, first token at +388 ms. */
export const WIRE_GENERATION = 'c1c2c3c4c5c6c7c8';
/** Its tool call, which the `type=tool` filter finds the trace by. */
export const WIRE_TOOL = 'd1d2d3d4d5d6d7d8';
/** Its guardrail — prompt `team@acme/answer`, an `@` inside a name and no version. */
export const WIRE_GUARDRAIL = 'e1e2e3e4e5e6e7e8';

/**
 * A section's link in the shell's navigation, wherever it is: in the column
 * on a desktop, and on a phone in the tab bar or — for the screens that are
 * not tabs — in the *More* sheet, which this opens (spec 006 #20).
 */
export async function section(page: Page, name: string): Promise<Locator> {
	const nav = page.getByRole('navigation', { name: 'Sections', exact: true });
	// The tabs and *More* render together, so once one link is up all are.
	await expect(nav.getByRole('link').first()).toBeVisible();
	const link = nav.getByRole('link', { name, exact: true });
	const more = nav.getByRole('button', { name: 'More', exact: true });
	// A desktop's column holds every section, and a phone's bar its tabs: a
	// name that is in neither fails on the link, not on a *More* that is not there.
	if ((await link.count()) || !(await more.count())) return link;
	await more.click();
	const sheet = page.getByRole('navigation', { name: 'More sections' });
	return sheet.getByRole('link', { name, exact: true });
}

/**
 * How far a table's own box would scroll sideways: 0 once a listing folds to
 * fit it (spec 006 #22). The box is the nearest ancestor that scrolls.
 */
export async function sideways(table: Locator): Promise<number> {
	return table.evaluate((node) => {
		let box = node.parentElement;
		while (box && getComputedStyle(box).overflowX === 'visible') box = box.parentElement;
		return box ? box.scrollWidth - box.clientWidth : 0;
	});
}

/** The column the sidebar takes from a desktop window (`w-52`), so a table's box is the window less this. */
const SIDEBAR = 208;
/**
 * Where the sidebar becomes tabs (`md`): a window this narrow has no column to subtract.
 * Kept apart from the app's own `PHONE` query on purpose: a test that reads its
 * numbers from the code it checks cannot see the code's numbers move.
 */
const MD = 768;

/**
 * A table at its own width and one rem under it (spec 006 #22, #24): in a box
 * of `box` px it has all `columns` and no sideways scroll, in one 16 px
 * narrower it has folded, and widening it again brings them back. The window
 * is resized *after* the table is on the screen, which is what a resized
 * window, or a tablet turned round, does to it. `around` is what the page
 * puts between the window and the box besides the sidebar: a card's padding
 * and border, a page's gutter. `tall` is the tallest a row may be in the whole
 * table: the layout at its own width is the honest one, and a value torn
 * across lines there (a timestamp, a key, an id) makes its row taller than the
 * table's ordinary rows are (spec 006 #22).
 */
export async function foldsAt(
	page: Page,
	table: Locator,
	box: number,
	columns: number,
	around = 0,
	tall = Infinity
) {
	const window = (px: number) =>
		page.setViewportSize({
			width: px + around + (px + around + SIDEBAR >= MD ? SIDEBAR : 0),
			height: 800
		});
	await window(box);
	await expect(table.locator('thead th')).toHaveCount(columns);
	expect(await sideways(table)).toBeLessThanOrEqual(0);
	const heights = await table
		.locator('tbody tr')
		.evaluateAll((rows) => rows.map((row) => row.getBoundingClientRect().height));
	expect(Math.max(...heights), 'the tallest row').toBeLessThanOrEqual(tall);

	await window(box - 16);
	await expect(table.locator('thead th')).not.toHaveCount(columns);
	expect(await sideways(table)).toBeLessThanOrEqual(0);

	await window(box + 16);
	await expect(table.locator('thead th')).toHaveCount(columns);
	expect(await sideways(table)).toBeLessThanOrEqual(0);
}

/**
 * Whether any folded line in the table is cut short of what it says: a line
 * meant to wrap between its values that is clipped instead. `sideways` cannot
 * see it, since clipped content adds nothing to a box's scroll width.
 */
export async function clipped(table: Locator): Promise<string[]> {
	return table.evaluate((node) =>
		[...node.querySelectorAll<HTMLElement>('tbody span[title]')]
			.filter((one) => {
				const cell = one.closest('td')!;
				const room = cell.getBoundingClientRect().right - parseFloat(getComputedStyle(cell).paddingRight);
				return one.scrollWidth > one.clientWidth || one.getBoundingClientRect().right > room + 1;
			})
			.map((one) => one.title)
	);
}
