import { expect, test, type Page } from '@playwright/test';
import { FAILING_TRACE, LARGE_PAYLOAD_OBSERVATION, LARGE_PAYLOAD_TRACE, state } from './harness';

// The scenario spec 006 asks for, end to end against the real binary: land on
// login, sign in through the URL the server printed, see the ingested traces,
// open one, walk its tree, load a payload the budget refused, and come back to
// the same observation from a deep link.

// Read inside the tests, never at module scope: Playwright collects the test
// files before it runs the global setup that writes this.
/** Signing in the way the first-run link does. */
async function signIn(page: Page) {
	await page.goto(state().preAuthed);
	await expect(page).toHaveURL(/\/traces$/);
}

test('an unauthenticated visit lands on the login form', async ({ page }) => {
	await page.goto('/traces');

	await expect(page).toHaveURL(/\/login\?next=/);
	await expect(page.getByLabel('Project key')).toBeVisible();
});

test('the pre-authed URL signs in and leaves the address bar', async ({ page }) => {
	await signIn(page);

	// The fragment carried a secret; it must not survive in the URL or in
	// history (spec 006 #8).
	expect(page.url()).not.toContain('#key=');
	await page.goBack();
	expect(page.url()).not.toContain('#key=');

	// And the key outlived the navigation, so a reload stays signed in.
	await page.goto('/traces');
	await expect(page.getByRole('heading', { name: 'Traces' })).toBeVisible();
});

test('a bad key is refused with a reason', async ({ page }) => {
	await page.goto('/login');
	await page.getByLabel('Project key').fill('tp-sk-not-a-real-key');
	await page.getByRole('button', { name: 'Sign in' }).click();

	await expect(page.getByRole('alert')).toContainText('did not accept');
	await expect(page).toHaveURL(/\/login/);
});

test('the ingested traces are on the list and link to themselves', async ({ page }) => {
	await signIn(page);

	await expect(page.getByRole('link', { name: /\d/ }).first()).toBeVisible();
	await expect(page.getByText('summarise-release-notes')).toBeVisible();
	// A failing trace says so in words, not only in colour.
	await expect(page.getByText('2 errors')).toBeVisible();
});

test('a filter narrows the listing and stays in the URL', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?environment=prod');

	await expect(page.getByText('answer-question')).toBeVisible();
	await expect(page.getByText('summarise-release-notes')).toHaveCount(0);
	await expect(page.getByRole('button', { name: /Remove filter Environment: prod/ })).toBeVisible();
});

test('a trace opens as a tree whose failures reach the root', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces/${FAILING_TRACE}`);

	await expect(page.getByText('failed in this trace')).toBeVisible();
	const items = page.getByRole('treeitem');
	await expect(items).toHaveCount(5);
	// The root did not fail itself, but something under it did.
	await expect(items.first().getByLabel('Something inside this failed')).toBeVisible();
});

test('the arrow keys walk the tree and the URL follows', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces/${FAILING_TRACE}`);

	await page.getByRole('treeitem').first().focus();
	await page.keyboard.press('ArrowDown');

	await expect(page).toHaveURL(/\?obs=[0-9a-f]{16}/);
	await expect(page.getByRole('treeitem', { selected: true })).toContainText('tool.search');

	// Left closes the branch rather than leaving it.
	await page.keyboard.press('ArrowUp');
	await page.keyboard.press('ArrowLeft');
	await expect(page.getByRole('treeitem')).toHaveCount(1);
});

test('a payload the budget refused is fetched on request', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces/${LARGE_PAYLOAD_TRACE}?obs=${LARGE_PAYLOAD_OBSERVATION}`);

	// The marker: a preview and the size of what is missing (spec 004 #2).
	const load = page.getByRole('button', { name: /Load the whole/ });
	await expect(load).toBeVisible();
	await load.click();

	// Swapped for the real value, rendered as a tree.
	await expect(load).toHaveCount(0);
	await expect(page.getByText('role:').first()).toBeVisible();
});

test('a deep link reloads to the same observation', async ({ page }) => {
	await signIn(page);
	const deep = `/traces/${LARGE_PAYLOAD_TRACE}?obs=${LARGE_PAYLOAD_OBSERVATION}`;

	// Asserted on the detail panel rather than on the tree: a phone shows one
	// pane at a time, and a link naming an observation opens on that one.
	const opened = page.getByRole('heading', { level: 2, name: 'summarise' });

	await page.goto(deep);
	await expect(opened).toBeVisible();

	await page.reload();
	await expect(opened).toBeVisible();
	await expect(page).toHaveURL(new RegExp(`obs=${LARGE_PAYLOAD_OBSERVATION}$`));
});

test('both themes render, and neither leaves the page scrolling sideways', async ({ page }) => {
	await signIn(page);

	const background = async () =>
		page.evaluate(() => getComputedStyle(document.body).backgroundColor);
	const overflow = async () =>
		page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);

	await page.emulateMedia({ colorScheme: 'light' });
	const light = await background();
	expect(await overflow()).toBeLessThanOrEqual(0);

	await page.emulateMedia({ colorScheme: 'dark' });
	const dark = await background();
	expect(await overflow()).toBeLessThanOrEqual(0);

	expect(dark).not.toBe(light);
});

test('the interface reaches no origin but its own', async ({ page }) => {
	// Fonts are bundled and nothing phones home (spec 006 #5).
	const foreign: string[] = [];
	page.on('request', (request) => {
		if (!request.url().startsWith(state().baseURL)) foreign.push(request.url());
	});

	await signIn(page);
	await page.goto(`/traces/${LARGE_PAYLOAD_TRACE}`);

	expect(foreign).toEqual([]);
});
