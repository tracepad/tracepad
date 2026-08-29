import { expect, test, type Page } from '@playwright/test';
import { state } from './harness';

// Turning pages against the real binary (spec 009). The corpus is seven
// traces, so the pages here are asked for through the URL — the API takes any
// size from 1 to 500 and the interface reads it, while the control in the bar
// offers the common steps.

async function signIn(page: Page) {
	await page.goto(state().preAuthed);
	await expect(page).toHaveURL(/\/traces$/);
}

const rows = (page: Page) => page.locator('tbody tr');

test('a page is turned, and turned back to the same rows', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?limit=2');
	await expect(rows(page)).toHaveCount(2);
	const first = await rows(page).allInnerTexts();

	await page.getByRole('button', { name: 'Next page' }).click();
	await expect(rows(page)).toHaveCount(2);
	expect(await rows(page).allInnerTexts()).not.toEqual(first);
	await expect(page).toHaveURL(/cursor=/);

	await page.getByRole('button', { name: 'Previous page' }).click();
	await expect(rows(page)).toHaveCount(2);
	expect(await rows(page).allInnerTexts()).toEqual(first);
});

test('the newest page has nowhere back, the oldest nowhere on', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?limit=2');

	await expect(page.getByRole('button', { name: 'Previous page' })).toBeDisabled();
	await expect(page.getByRole('button', { name: 'Newest page' })).toBeDisabled();
	await expect(page.getByRole('button', { name: 'Next page' })).toBeEnabled();

	// Straight to the far end: a direction, not an offset (spec 009 #2).
	await page.getByRole('button', { name: 'Oldest page' }).click();
	await expect(page).toHaveURL(/direction=prev/);
	await expect(page.getByRole('button', { name: 'Next page' })).toBeDisabled();
	await expect(page.getByRole('button', { name: 'Previous page' })).toBeEnabled();
});

test('a page survives a reload, because it is in the URL', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?limit=2');
	await page.getByRole('button', { name: 'Next page' }).click();
	const deep = page.url();
	const shown = await rows(page).allInnerTexts();

	await page.reload();

	await expect(page).toHaveURL(deep);
	expect(await rows(page).allInnerTexts()).toEqual(shown);
});

test('the bar counts what the filters match, not what is on the page', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?limit=2');

	// Seven in the corpus, two on screen.
	await expect(page.getByText('2 of 7 traces')).toBeVisible();

	await page.goto('/traces?limit=2&environment=staging');
	await expect(page.getByText(/of 1 trace\b/)).toBeVisible();
});

test('changing the page size starts again at the newest page', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?limit=2');
	await page.getByRole('button', { name: 'Next page' }).click();
	await expect(page).toHaveURL(/cursor=/);

	await page.getByLabel('Rows per page').selectOption('25');

	// A cursor is a position in one paging of one listing; at another size it
	// points into a page that no longer exists.
	await expect(page).not.toHaveURL(/cursor=/);
	await expect(rows(page)).toHaveCount(7);
});

test('j on the last row of a page turns it and keeps reading', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?limit=2');
	// Open the second — and last — row of the page.
	await rows(page).nth(1).getByRole('link').click();
	const panel = page.getByRole('dialog');
	await expect(panel).toBeVisible();
	const opened = new URL(page.url()).searchParams.get('peek');

	await page.keyboard.press('j');

	// The page turned under the panel, and the panel moved on rather than
	// stopping at a boundary that is an artefact of paging (spec 009 #6).
	await expect(page).toHaveURL(/cursor=/);
	await expect(panel).toBeVisible();
	expect(new URL(page.url()).searchParams.get('peek')).not.toBe(opened);
	await expect(rows(page).first().getByRole('link')).toHaveAttribute('aria-current', 'true');
});

test('live is paused off the newest page and says so', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?limit=2&live=1');
	const live = page.getByRole('button', { name: 'Live' });
	await expect(live).toBeEnabled();

	await page.getByRole('button', { name: 'Next page' }).click();

	await expect(live).toBeDisabled();
	await expect(live).toHaveAttribute('title', /Paused/);
});

test('the sessions listing pages the same way', async ({ page }) => {
	await signIn(page);
	await page.goto('/sessions?limit=1');

	await expect(rows(page)).toHaveCount(1);
	await expect(page.getByText('1 of 2 sessions')).toBeVisible();
	await page.getByRole('button', { name: 'Next page' }).click();
	await expect(page).toHaveURL(/cursor=/);
	await expect(page.getByRole('button', { name: 'Next page' })).toBeDisabled();
});

test("a session's own traces page inside the panel", async ({ page }) => {
	await signIn(page);
	await page.goto('/sessions/session-77');

	// Its total is exact — the endpoint answers with it — so the bar says so
	// without asking for a count of its own.
	const bar = page.getByText(/of 1 trace\b/);
	await expect(bar).toBeVisible();
	await expect(page.getByRole('button', { name: 'Next page' })).toBeDisabled();
});
