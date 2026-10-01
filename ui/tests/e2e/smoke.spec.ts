import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';
import {
	BCRYPT_WAIT,
	FAILING_TRACE,
	fromOwnAddress,
	LARGE_PAYLOAD_OBSERVATION,
	LARGE_PAYLOAD_TRACE,
	signIn as enter,
	signInAsMember,
	state
} from './harness';

// The scenario spec 006 asks for, end to end against the real binary: land on
// login, sign in, see the ingested traces, open one, walk its tree, load a
// payload the budget refused, and come back to the same observation from a
// deep link.

// Read inside the tests, never at module scope: Playwright collects the test
// files before it runs the global setup that writes this.
async function signIn(page: Page) {
	await signInAsMember(page);
}

test('an unauthenticated visit lands on the login form', async ({ page }) => {
	await page.goto('/traces');

	await expect(page).toHaveURL(/\/login\?next=/);
	await expect(page.getByLabel('Email')).toBeVisible();
});

test('a session outlives the navigation', async ({ page }) => {
	await signIn(page);

	// The credential is a cookie the browser holds, so a reload is still
	// signed in and nothing about it was ever in the address bar (spec 028 #4).
	await page.goto('/traces');
	await expect(page.getByRole('heading', { name: 'Traces' })).toBeVisible();
});

test('a wrong password is refused with one sentence', async ({ page }) => {
	await fromOwnAddress(page);
	await page.goto('/login');
	await page.getByLabel('Email').fill(state().member.email);
	await page.getByLabel('Password', { exact: true }).fill('not-the-password');
	await page.getByRole('button', { name: 'Sign in' }).click();

	// One text for every way of failing, so the form enumerates nobody (#8).
	await expect(page.getByRole('alert')).toContainText('wrong email or password', { timeout: BCRYPT_WAIT });
	await expect(page).toHaveURL(/\/login/);
});

test('an unknown email is refused with the same sentence', async ({ page }) => {
	await fromOwnAddress(page);
	await page.goto('/login');
	await page.getByLabel('Email').fill('nobody@e2e.test');
	await page.getByLabel('Password', { exact: true }).fill('not-the-password');
	await page.getByRole('button', { name: 'Sign in' }).click();

	await expect(page.getByRole('alert')).toContainText('wrong email or password', { timeout: BCRYPT_WAIT });
});

test('signing out ends the session and the next screen asks again', async ({ page }) => {
	// Through the form, on a session of its own: signing out ends the session
	// server-side, and the one `signInAsMember` hands out is every other test's.
	await enter(page, state().member);

	await page.getByRole('button', { name: /^Signed in as/ }).click();
	await page.getByRole('menuitem', { name: 'Sign out' }).click();

	await expect(page).toHaveURL(/\/login/);
	await page.goto('/traces');
	await expect(page).toHaveURL(/\/login\?next=/);
});

test('the ingested traces are on the list and link to themselves', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces');

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

	// The marker, as the banner over its preview: how much is here, how much
	// there is, and the click that spends the budget on the rest (spec 004 #2,
	// spec 015 #3).
	const load = page.getByRole('button', { name: /Load the whole/ });
	await expect(load).toBeVisible();
	await load.click();

	// Swapped for the whole payload, in the document surface (spec 015 #1).
	await expect(load).toHaveCount(0);
	await expect(page.getByLabel('Input', { exact: true })).toContainText('"content"');
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

test('another trace starts on the tree, not on the last one’s pane', async ({ page }) => {
	// Only a phone shows one pane at a time; on a wide screen both are up and
	// there is nothing to carry over.
	test.skip(test.info().project.name !== 'mobile', 'the panes only alternate on a phone');
	await signIn(page);

	await page.goto(`/traces/${LARGE_PAYLOAD_TRACE}?obs=${LARGE_PAYLOAD_OBSERVATION}`);
	await expect(page.getByRole('tab', { name: 'Observation' })).toHaveAttribute(
		'aria-selected',
		'true'
	);

	// SvelteKit reuses the component across trace ids; the previous trace's
	// pane says nothing about this one.
	await page.goto(`/traces/${FAILING_TRACE}`);

	await expect(page.getByRole('tab', { name: 'Tree' })).toHaveAttribute('aria-selected', 'true');
	await expect(page.getByRole('treeitem').first()).toBeVisible();
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
