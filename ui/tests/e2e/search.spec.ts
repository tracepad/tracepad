import { expect, test, type Page } from '@playwright/test';
import { state } from './harness';

// Search, end to end against the real binary (spec 011, Testing): a phrase
// seeded into a fixture payload, the snippet under the row, the click that
// lands on the observation that matched, and the empty state that names the
// query back.

/** The fixture whose generation carries "how do I reset my password?". */
const CHAT_TRACE = '4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f';
const CHAT_GENERATION = '2b3c4d5e6f7a8b9c';

async function signIn(page: Page) {
	await page.goto(state().preAuthed);
	await expect(page).toHaveURL(/\/traces$/);
}

test('a search narrows the listing and says where it matched', async ({ page }) => {
	await signIn(page);

	await page.getByLabel('Search prompts, answers and errors').fill('reset my password');
	await page.keyboard.press('Enter');

	// The search is in the URL, so it is a link somebody can send.
	await expect(page).toHaveURL(/[?&]q=reset\+my\+password/);
	await expect(page.locator('tbody')).toHaveCount(1);
	await expect(page.getByRole('link', { name: /\d/ }).first()).toBeVisible();

	// The snippet is under the row, with the query's words marked.
	const snippet = page.locator('tbody td[colspan]');
	await expect(snippet).toContainText('input');
	await expect(snippet).toContainText('password');
	await expect(snippet.locator('mark').first()).toHaveText(/reset|my|password/i);

	// And it survives a reload, because the whole listing is in the URL.
	await page.reload();
	await expect(page.locator('tbody td[colspan]')).toContainText('password');
});

test('clicking a match opens the panel on the observation that matched', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?q=password');

	await page.locator('tbody tr').first().click();

	await expect(page).toHaveURL(new RegExp(`peek=${CHAT_TRACE}`));
	await expect(page).toHaveURL(new RegExp(`obs=${CHAT_GENERATION}`));
	const panel = page.getByRole('dialog');
	await expect(panel).toBeVisible();
	await expect(panel).toContainText('how do I reset my password?');
});

test('a search that matches nothing names the query back', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?q=aardvark');

	await expect(page.getByRole('heading', { name: /Nothing matches/ })).toContainText('aardvark');

	await page.getByRole('button', { name: 'Clear the search', exact: true }).click();
	await expect(page).not.toHaveURL(/q=/);
	await expect(page.locator('tbody tr').first()).toBeVisible();
});

test('the search box clears itself and the listing comes back', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?q=password');
	const narrowed = await page.locator('tbody').count();

	await page.getByRole('button', { name: 'Clear the search box' }).click();

	await expect(page).not.toHaveURL(/q=/);
	expect(await page.locator('tbody').count()).toBeGreaterThan(narrowed);
});
