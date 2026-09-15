import { expect, type Page } from '@playwright/test';
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
export const ADMIN_TOKEN = 'e2e-admin-token';

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
	memberships: { project_id: string; role: 'editor' }[]
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
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ token, password: PASSWORD })
	});
	if (!response.ok) throw new Error(`accept the invitation: ${response.status}`);
}

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
	await page.goto('/login');
	await page.getByLabel('Email').fill(account.email);
	// Exact: the eye beside the field is labelled "Show the password".
	await page.getByLabel('Password', { exact: true }).fill(account.password);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).toHaveURL(/\/p\/[0-9a-f]{32}\/dashboard(\?|$)/);
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
