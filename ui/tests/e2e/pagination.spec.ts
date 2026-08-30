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

/**
 * The top row's link, whose href carries the trace id: the identity of the
 * page on screen.
 *
 * A page turn does not empty the table — `Listing` keeps the rows it has until
 * the next page lands (`listing.svelte.ts`, `#load`) — so a count of two is
 * equally true of the page being left, and reading the rows on it is reading
 * the page before the turn. Every test here that compares rows across a turn
 * waits for this to change first.
 */
const top = (page: Page) => rows(page).first().getByRole('link');
const identity = (page: Page) => top(page).getAttribute('href');

test('a page is turned, and turned back to the same rows', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?limit=2');
	await expect(rows(page)).toHaveCount(2);
	const first = await rows(page).allInnerTexts();
	const newest = (await identity(page)) ?? '';

	await page.getByRole('button', { name: 'Next page' }).click();
	await expect(top(page)).not.toHaveAttribute('href', newest);
	await expect(rows(page)).toHaveCount(2);
	expect(await rows(page).allInnerTexts()).not.toEqual(first);
	await expect(page).toHaveURL(/cursor=/);

	await page.getByRole('button', { name: 'Previous page' }).click();
	await expect(top(page)).toHaveAttribute('href', newest);
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
	await expect(rows(page)).toHaveCount(2);
	const newest = (await identity(page)) ?? '';

	await page.getByRole('button', { name: 'Next page' }).click();
	await expect(top(page)).not.toHaveAttribute('href', newest);
	const deep = page.url();
	const shown = await rows(page).allInnerTexts();

	await page.reload();

	await expect(page).toHaveURL(deep);
	// A reload starts from an empty table, so the rows are the reloaded page's
	// as soon as there are any.
	await expect(rows(page)).toHaveCount(shown.length);
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
	// The walk moves when the turned page lands, not when its URL appears, and
	// the row it lights is what says it has: read before that, `peek` is still
	// the row the walk started on.
	await expect(top(page)).toHaveAttribute('aria-current', 'true');
	expect(new URL(page.url()).searchParams.get('peek')).not.toBe(opened);
});

test('a walk from a row this page does not hold takes the nearest one', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?limit=2');

	// A link somebody sent: a page, and a panel open on a trace that is not on
	// it — the state retention and a live tick also leave behind.
	await rows(page).nth(1).getByRole('link').click();
	const behind = new URL(page.url()).searchParams.get('peek') ?? '';
	await page.keyboard.press('Escape');
	await page.getByRole('button', { name: 'Next page' }).click();
	await expect(page).toHaveURL(/cursor=/);
	const deep = new URL(page.url());
	deep.searchParams.set('peek', behind);

	await page.goto(deep.toString());
	// Until the panel's row says where it sits, the walk deliberately does not
	// move, so this waits for the detail rather than for the panel.
	await expect(page.getByRole('button', { name: 'Copy the trace id' })).toBeVisible();
	await expect(page.locator('tbody [aria-current="true"]')).toHaveCount(0);

	// Every row here is older than the one in the panel, so the nearest one
	// older — what `j` asks for — is the *first*. Landing on the oldest row on
	// screen, as an edge-guessing walk did, skipped the page (spec 009 #13).
	await page.keyboard.press('j');
	await expect(rows(page).first().getByRole('link')).toHaveAttribute('aria-current', 'true');
	// On this page, not by turning it: the row it wanted was here all along.
	expect(new URL(page.url()).searchParams.get('cursor')).toBe(deep.searchParams.get('cursor'));
	expect(new URL(page.url()).searchParams.get('peek')).not.toBe(behind);
});

test('live is paused off the newest page, and can still be switched off', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?limit=2&live=1');
	const live = page.getByRole('button', { name: 'Live' });
	await expect(live).toHaveAttribute('title', /Re-read/);

	await page.getByRole('button', { name: 'Next page' }).click();
	await expect(live).toHaveAttribute('title', /Paused/);

	// Disabling the toggle here would disable the only control that can unset
	// `live=1`, which the page turn carried along (PR #11 review).
	await expect(live).toBeEnabled();
	await live.click();
	await expect(page).not.toHaveURL(/live=1/);
});

test('a listing that fits on one page has all four controls dead', async ({ page }) => {
	await signIn(page);
	// Seven traces at the default size: this page is both ends at once. A
	// live » here would navigate to `?direction=prev`, show the same rows and
	// quietly pause live mode (PR #11, third review).
	await expect(page.locator('tbody tr')).toHaveCount(7);
	for (const name of ['Newest page', 'Previous page', 'Next page', 'Oldest page']) {
		await expect(page.getByRole('button', { name })).toBeDisabled();
	}
});

test('walking back to a short page still leaves a way home', async ({ page }) => {
	await signIn(page);
	// Seven traces in pages of two: from the far end, walking back lands on
	// a one-row page with nothing above it — rows, but no `prev_cursor`. « is
	// an anchor and must stay live there, or the reader is stuck on one row
	// with live mode paused (PR #11, fourth review).
	await page.goto('/traces?limit=2&direction=prev');
	for (let step = 0; step < 3; step++) {
		// One turn at a time. The cursors in the bar belong to the page on
		// screen, so a click before the next one lands would re-address the
		// page just asked for and quietly lose a turn — the bar goes dead
		// while a page is in flight, and this waits the same way (PR #11).
		const here = page.url();
		await page.getByRole('button', { name: 'Previous page' }).click();
		await expect(page).not.toHaveURL(here);
	}
	await expect(page.locator('tbody tr')).toHaveCount(1);
	await expect(page.getByRole('button', { name: 'Previous page' })).toBeDisabled();

	const home = page.getByRole('button', { name: 'Newest page' });
	await expect(home).toBeEnabled();
	await home.click();
	await expect(page).toHaveURL(/\/traces\?limit=2$/);
	await expect(page.locator('tbody tr')).toHaveCount(2);
});

test('an empty page off the newest one is not a dead end', async ({ page }) => {
	await signIn(page);
	// A filter that matches nothing, on the oldest page: no rows, and so no
	// cursors either — the state where every *step* is impossible and only
	// the anchors can help (PR #11, second review).
	await page.goto('/traces?limit=2&direction=prev&environment=nowhere');

	await expect(page.locator('tbody tr')).toHaveCount(0);
	await expect(page.getByRole('button', { name: 'Previous page' })).toBeDisabled();
	await expect(page.getByRole('button', { name: 'Next page' })).toBeDisabled();

	// « is an anchor, not a step: it needs no cursor and is the way out.
	const newest = page.getByRole('button', { name: 'Newest page' });
	await expect(newest).toBeEnabled();
	await newest.click();
	await expect(page).not.toHaveURL(/direction=prev/);
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
