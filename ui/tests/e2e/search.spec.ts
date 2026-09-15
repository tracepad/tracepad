import { expect, test, type Page } from '@playwright/test';
import { signIn as enter, state } from './harness';

// Search, end to end against the real binary (spec 011, Testing): a phrase
// seeded into a fixture payload, the snippet under the row, the click that
// lands on the observation that matched, and the empty state that names the
// query back.

/** The fixture whose generation carries "how do I reset my password?". */
const CHAT_TRACE = '4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f';
const CHAT_GENERATION = '2b3c4d5e6f7a8b9c';

/** Signs in and opens the listing: the front page is the dashboard (spec 034 #1). */
async function signIn(page: Page) {
	await enter(page, state().member);
	await page.goto('/traces');
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
	// Both counts are read once their listing is on screen. `count()` waits
	// for nothing, and a table still loading has no rows: read too early, the
	// narrowed count is 0 and "more than before" is true of anything.
	await expect(page.locator('tbody td[colspan]').first()).toBeVisible();
	const narrowed = await page.locator('tbody').count();

	await page.getByRole('button', { name: 'Clear the search box' }).click();

	await expect(page).not.toHaveURL(/q=/);
	await expect(page.locator('tbody')).not.toHaveCount(narrowed);
	expect(await page.locator('tbody').count()).toBeGreaterThan(narrowed);
});

// ✕ after typing must discard what was typed. The box commits on blur, and
// clicking the button blurs it, so without care the click ran the very search
// it was pressed to throw away — and unmounted the button on the way, so the
// click never reached it (found in review of PR #16).
test('clearing after typing discards the text instead of searching for it', async ({ page }) => {
	await signIn(page);
	// `signIn` waits for the URL, which is not the listing: the count of what
	// the screen holds is taken once it holds it.
	await expect(page.locator('tbody tr').first()).toBeVisible();
	const all = await page.locator('tbody').count();

	await page.getByLabel('Search prompts, answers and errors').fill('aardvark');
	await page.getByRole('button', { name: 'Clear the search box' }).click();

	await expect(page).not.toHaveURL(/q=/);
	await expect(page.getByLabel('Search prompts, answers and errors')).toHaveValue('');
	expect(await page.locator('tbody').count()).toBe(all);
});

test('clearing an active search after typing over it turns one page, not two', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?q=password');
	// The counting starts once the page's own search is on screen. `goto`
	// returns on the load event, which is not when the application has asked
	// for its listing: on a slow machine that request is still to come, and
	// counted here it looks like a second page turn. CI is such a machine —
	// this is what made it red on `main`, and a CPU throttled 20× reproduces
	// it on either side of the merge it was blamed on.
	await expect(page.locator('tbody td[colspan]').first()).toBeVisible();

	const listings: string[] = [];
	page.on('request', (request) => {
		if (request.url().includes('/api/v1/traces?')) listings.push(request.url());
	});

	await page.getByLabel('Search prompts, answers and errors').fill('aardvark');
	await page.getByRole('button', { name: 'Clear the search box' }).click();
	await expect(page).not.toHaveURL(/q=/);
	// And the counting ends once the cleared listing is on screen. A visible
	// row is not that: `Listing` leaves the rows it has while the next page
	// loads, so the searched row — snippet cell and all — is still there, and
	// waiting for it waits for nothing (found in review of this PR). The
	// snippet cells are what only the search had.
	await expect(page.locator('tbody tr').first()).toBeVisible();
	await expect(page.locator('tbody td[colspan]')).toHaveCount(0);

	// One listing and its count, for the one page turn ✕ asked for — never a
	// search for "aardvark" on the way.
	expect(listings.filter((url) => url.includes('aardvark'))).toEqual([]);
	expect(listings.filter((url) => !url.includes('count=1'))).toHaveLength(1);
});
