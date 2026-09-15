import { expect, test, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { spawn } from 'node:child_process';
import { mkdtempSync, readdirSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { FAILING_TRACE, PASSWORD } from './harness';

// The whole of spec 028 as one story, against a server of its own: the setup
// link the binary printed creates the first owner, that owner invites a
// viewer, the viewer accepts the invitation in another browser, sees the
// project and no Server tab, scores a trace and is offered no prompt editor —
// and when the owner disables them, their next navigation lands on `/login`.
//
// A server of its own because the setup link exists once per server: the token
// is minted at each start *while no enabled owner exists* (Decision 9), so the
// shared boot in `global-setup.ts` has already spent it by the time any test
// runs. Everything else in this suite runs on that shared server; only this
// file needs a machine nobody has set up yet.
//
// Desktop only, for the same reason: the story is about who may do what, not
// about how it lays out, and a second server per Playwright project would buy
// nothing with it.

const ONLY = 'desktop';
const PORT = Number(process.env.TRACEPAD_E2E_ACCOUNTS_PORT ?? 47323);
const ROOT = resolve(process.cwd(), '..');

const OWNER = { email: 'founder@e2e.test', password: PASSWORD };
const VIEWER = { email: 'helper@e2e.test', password: 'viewer-password' };

type Stand = { base: string; setup: string; key: string; project: string; stop: () => void };

let stand: Stand | null = null;
/** The invitation link the owner is handed, carried from one test to the next. */
let invitation = '';

function boot(): Promise<Stand> {
	const dataDir = mkdtempSync(join(tmpdir(), 'tracepad-accounts-'));
	const server = spawn(join(ROOT, 'bin', 'tracepad'), ['serve', '--listen', `127.0.0.1:${PORT}`], {
		env: { ...process.env, TRACEPAD_DATA_DIR: dataDir, TRACEPAD_ROLLUP_INTERVAL: '1s' },
		stdio: ['ignore', 'pipe', 'pipe']
	});
	const stop = () => {
		server.kill('SIGTERM');
		rmSync(dataDir, { recursive: true, force: true });
	};
	return new Promise((accept, reject) => {
		let output = '';
		const timer = setTimeout(() => {
			stop();
			reject(new Error(`the server printed no first-run output:\n${output}`));
		}, 20_000);
		const read = (chunk: Buffer) => {
			output += chunk.toString();
			const key = /authorization=Bearer (tp-sk-[0-9a-f]+)/.exec(output);
			const setup = /http:\/\/\S+\/setup#token=\S+/.exec(output);
			if (!key || !setup) return;
			clearTimeout(timer);
			const base = `http://127.0.0.1:${PORT}`;
			accept({
				base,
				// The server names itself `localhost`; the tests drive
				// `127.0.0.1`, and a cross-origin hop would drop the cookie.
				setup: setup[0].replace(/^http:\/\/[^/]+/, base),
				key: key[1],
				project: '',
				stop
			});
		};
		server.stdout?.on('data', read);
		server.stderr?.on('data', read);
		server.on('exit', (code) => {
			clearTimeout(timer);
			reject(new Error(`the server exited with ${code}:\n${output}`));
		});
	});
}

/** One request as the project key, which is how this file seeds what it reads. */
async function seed(at: Stand) {
	for (let attempt = 0; attempt < 100; attempt++) {
		try {
			if ((await fetch(`${at.base}/health`)).ok) break;
		} catch {
			// Not listening yet.
		}
		await new Promise((wake) => setTimeout(wake, 100));
	}
	const fixtures = join(ROOT, 'testdata', 'otlp');
	for (const name of readdirSync(fixtures).filter((file) => file.endsWith('.pb'))) {
		const response = await fetch(`${at.base}/v1/traces`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/x-protobuf', Authorization: `Bearer ${at.key}` },
			body: readFileSync(join(fixtures, name))
		});
		if (!response.ok) throw new Error(`ingest ${name}: ${response.status}`);
	}
	// One prompt, so that "a viewer is offered no editor" has something to be
	// offered it on.
	const written = await fetch(`${at.base}/api/v1/prompts/support-answer/versions`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${at.key}` },
		body: JSON.stringify({ type: 'text', prompt: 'Answer the question.' })
	});
	if (!written.ok) throw new Error(`seed the prompt: ${written.status}`);
	const projects = await fetch(`${at.base}/api/v1/projects`, {
		headers: { Authorization: `Bearer ${at.key}` }
	});
	const { projects: rows } = (await projects.json()) as { projects: { id: string }[] };
	at.project = rows[0].id;
}

/** Signs in through the form, on this file's own server. */
async function signIn(page: Page, at: Stand, who: { email: string; password: string }) {
	await page.goto(`${at.base}/login`);
	await page.getByLabel('Email').fill(who.email);
	await page.getByLabel('Password', { exact: true }).fill(who.password);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).toHaveURL(/\/dashboard(\?|$)/);
}

/**
 * A second browser, for the one test that needs two people signed in at once.
 *
 * Closed at the end of the file rather than at the end of the test: a context
 * whose page is on a live screen takes its time going, and the story does not
 * care — the server it was talking to is stopped first.
 */
const extra: BrowserContext[] = [];

async function otherBrowser(browser: Browser) {
	const context = await browser.newContext();
	extra.push(context);
	return context.newPage();
}

test.describe('accounts, from the link the server printed', () => {
	// Each step is the state the next one needs, so they run in order and in
	// one worker.
	test.describe.configure({ mode: 'serial' });

	test.beforeEach(({}, info) => {
		test.skip(info.project.name !== ONLY, 'one server for this story is enough');
		// Each of these walks several screens in two browsers, and the third
		// also boots the story's own server behind it.
		test.slow();
	});

	test.beforeAll(async ({}, info) => {
		if (info.project.name !== ONLY) return;
		stand = await boot();
		await seed(stand);
	});

	test.afterAll(async () => {
		stand?.stop();
		stand = null;
		await Promise.all(extra.splice(0).map((context) => context.close()));
	});

	test('the printed link creates the first owner and signs them in', async ({ page }) => {
		const at = stand as Stand;

		// Before the owner exists, every screen is the setup screen — and
		// without the token there is nothing on it to fill in. On a page of
		// its own, because a `goto` that changes only the fragment is a
		// same-document navigation, and the screen would never be built.
		const guard = await page.context().newPage();
		await guard.goto(`${at.base}/traces`);
		await expect(guard).toHaveURL(/\/setup$/);
		await expect(guard.getByRole('button', { name: 'Create the owner' })).toHaveCount(0);
		await guard.close();

		await page.goto(at.setup);
		await page.getByLabel('Display name').fill('The Founder');
		await page.getByLabel('Email').fill(OWNER.email);
		await page.getByLabel('Password', { exact: true }).fill(OWNER.password);
		await page.getByLabel('Password again').fill(OWNER.password);
		await page.getByRole('button', { name: 'Create the owner' }).click();

		await expect(page).toHaveURL(/\/dashboard(\?|$)/);
		// The token rode in the fragment and does not survive the screen
		// (spec 006 #8), nor the back button.
		expect(page.url()).not.toContain('#token=');
		await page.goBack();
		expect(page.url()).not.toContain('#token=');

		// And the link is spent: the token still parses, but the server has an
		// owner now, so there is nothing to set up.
		await page.goto(at.setup);
		await expect(page.getByText('already has an owner')).toBeVisible();
		// Without a token at all it says the other thing, and both point at
		// the sign-in form.
		await page.goto(`${at.base}/setup`);
		await expect(page.getByText('carries no setup token')).toBeVisible();
		await expect(page.getByRole('link', { name: 'Go to the sign-in form' })).toBeVisible();
	});

	test('the owner invites a viewer and is handed the link once', async ({ page }) => {
		const at = stand as Stand;
		await signIn(page, at, OWNER);
		await page.goto(`${at.base}/settings/server`);

		await page.getByRole('button', { name: 'Invite' }).click();
		const dialog = page.getByRole('dialog');
		await dialog.getByLabel('Email').fill(VIEWER.email);
		await dialog.getByLabel('Display name').fill('The Helper');
		await dialog.getByLabel(/^Role in /).selectOption('Viewer');
		await dialog.getByRole('button', { name: 'Invite' }).click();

		// Shown once, with a copy button and the expiry (spec 028 #10).
		const shown = page.getByRole('dialog').filter({ hasText: 'The invitation link' });
		await expect(shown).toContainText('only time the link is shown');
		invitation = ((await shown.locator('pre').textContent()) ?? '').trim();
		expect(invitation).toContain('/invite#token=');
		await shown.getByRole('button', { name: 'I have copied it' }).click();

		// And the row says where they stand before they have accepted.
		const row = page.getByRole('row').filter({ hasText: VIEWER.email });
		await expect(row).toContainText('pending');
		await expect(row).toContainText('never');
	});

	// A browser of its own, which is what an invitation link is opened in:
	// every test here gets a fresh context, so this one is the viewer's and
	// the owner's from the test before is gone.
	test('the viewer accepts in another browser and sees a viewer’s interface', async ({ page }) => {
		const at = stand as Stand;
		await page.goto(invitation.replace(/^http:\/\/[^/]+/, at.base));
		await page.getByLabel('Password', { exact: true }).fill(VIEWER.password);
		await page.getByLabel('Password again').fill(VIEWER.password);
		await page.getByRole('button', { name: 'Set the password and sign in' }).click();
		await expect(page).toHaveURL(/\/dashboard(\?|$)/);

		// The project's traces, which is what the membership is for.
		await page.goto(`${at.base}/traces`);
		await expect(page.getByText('summarise-release-notes')).toBeVisible();

		// No Server tab, and the address redirects (Decision 14).
		await page.goto(`${at.base}/settings/server`);
		await expect(page).toHaveURL(/\/settings\/project$/);
		await expect(page.getByRole('tab', { name: 'Server' })).toHaveCount(0);
		// One line per card that has lost its controls: the retention windows,
		// the keys and the danger zone.
		await expect(page.getByText('Your role in this project is viewer')).toHaveCount(3);

		// Scoring is a viewer's job — that is what the role is for (#3).
		await page.goto(`${at.base}/traces/${FAILING_TRACE}`);
		await page.getByRole('button', { name: 'Score', exact: true }).click();
		// Exact: the free-name field beside it is labelled "Score name".
		await page.getByLabel('Name', { exact: true }).selectOption('other…');
		await page.getByLabel('Score name').fill('verdict');
		await page.getByRole('radio', { name: 'numeric' }).check();
		await page.getByLabel('Value').fill('0.75');
		await page.getByRole('button', { name: 'Save' }).click();
		await expect(page.getByRole('button', { name: /verdict/ })).toContainText('0.75');

		// A user's page is readable and carries no erasure: the endpoint behind
		// that button is an editor's, wherever the button is put (#15).
		await page.goto(`${at.base}/users`);
		await page.getByRole('link').filter({ hasText: /\w/ }).last().click();
		await expect(page).toHaveURL(/\/users\/.+/);
		await expect(page.getByRole('button', { name: 'Erase data' })).toHaveCount(0);

		// And the prompt is readable with nothing on it to press (#15).
		await page.goto(`${at.base}/prompts/support-answer`);
		await expect(page.getByRole('heading', { name: 'support-answer' })).toBeVisible();
		await expect(page.getByRole('button', { name: 'New version' })).toHaveCount(0);
		await expect(page.getByRole('button', { name: 'Delete prompt' })).toHaveCount(0);
		await expect(page.getByRole('button', { name: 'Add label…' })).toHaveCount(0);
	});

	test('disabling the viewer ends their session at the next navigation', async ({
		page,
		browser
	}) => {
		const at = stand as Stand;
		// The viewer is on this test's own browser and the owner is beside
		// them in another, because the point is what happens to a tab that is
		// already open when somebody else changes what it may see.
		await signIn(page, at, VIEWER);
		const owner = await otherBrowser(browser);

		await signIn(owner, at, OWNER);
		await owner.goto(`${at.base}/settings/server`);
		const row = owner.getByRole('row').filter({ hasText: VIEWER.email });
		await expect(row).toContainText('active');
		await row.getByRole('button', { name: 'Edit' }).click();
		await owner.getByRole('dialog').getByRole('checkbox', { name: /^Disabled/ }).check();
		await owner.getByRole('dialog').getByRole('button', { name: 'Save' }).click();
		await expect(row).toContainText('disabled');

		// Their sessions ended with the flag (Decision 12), so the next screen
		// they ask for is the login form with where they were.
		await page.goto(`${at.base}/sessions`);
		await expect(page).toHaveURL(/\/login\?next=/);

		// And the password no longer opens it.
		await page.getByLabel('Email').fill(VIEWER.email);
		await page.getByLabel('Password', { exact: true }).fill(VIEWER.password);
		await page.getByRole('button', { name: 'Sign in' }).click();
		await expect(page.getByRole('alert')).toContainText('wrong email or password');
	});
});
