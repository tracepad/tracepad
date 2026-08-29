import { expect, test, type Page } from '@playwright/test';
import { state } from './harness';

// The peek panel (spec 008), end to end against the real binary: a row opens
// beside the listing it came from, the listing survives underneath, and what
// the panel shows is still a link somebody can send.

async function signIn(page: Page) {
	await page.goto(state().preAuthed);
	await expect(page).toHaveURL(/\/traces$/);
}

/** The rows of a listing, header excluded. */
const rows = (page: Page) => page.locator('tbody tr');

test('a trace row opens the panel and says which row it came from', async ({ page }) => {
	await signIn(page);
	const row = rows(page).filter({ hasText: 'summarise-release-notes' });
	await row.getByRole('link').click();

	const panel = page.getByRole('dialog');
	await expect(panel).toBeVisible();
	await expect(page).toHaveURL(/\/traces\?peek=[0-9a-f]{32}$/);
	await expect(panel.getByRole('treeitem').first()).toBeVisible();
	// With no scrim under it, the lit row is the only thing that says where
	// the panel's contents came from (spec 008 #10).
	await expect(row.getByRole('link')).toHaveAttribute('aria-current', 'true');
});

/**
 * The regression the PR #10 review caught: the listing's load effect depended
 * on the filter *object*, which `readFilters` rebuilds on every URL change —
 * so `?peek=` re-ran it, and the rows on screen were replaced by a fresh
 * first page. With two "Load more" pages behind it, opening row 120 threw
 * away both of them and the peeked row left the listing entirely. Counting
 * the requests is the assertion that survives a small fixture corpus.
 */
async function listingRequests(page: Page): Promise<() => number> {
	let count = 0;
	page.on('request', (request) => {
		const url = request.url();
		// The listing, not a trace (`/api/v1/traces?…` against `/traces/{id}`)
		// and not the capped count, which is its own request on its own
		// trigger (spec 009 #4).
		if (/\/api\/v1\/traces\?/.test(url) && !url.includes('count=1')) count++;
	});
	return () => count;
}

test('opening the panel does not re-read the listing under it', async ({ page }) => {
	await signIn(page);
	await expect(rows(page).first()).toBeVisible();
	const listings = await listingRequests(page);
	const before = listings();

	await rows(page).filter({ hasText: 'answer-question' }).getByRole('link').click();
	const panel = page.getByRole('dialog');
	await expect(panel.getByRole('treeitem').first()).toBeVisible();

	// Selecting a node writes `?obs=` onto the same URL: also not a filter.
	await panel.getByRole('treeitem').nth(1).click();
	await expect(page).toHaveURL(/obs=/);

	// And walking to the next row, and closing.
	await panel.getByRole('button', { name: 'Next row' }).click();
	await page.keyboard.press('Escape');
	await expect(page.getByRole('dialog')).toHaveCount(0);

	expect(listings() - before).toBe(0);
});

test('opening a session panel does not re-read the sessions listing', async ({ page }) => {
	await signIn(page);
	await page.goto('/sessions');
	// Counted only once the listing is on screen: the screen also asks for a
	// capped count (spec 009 #4), and starting the tally mid-flight would
	// catch that one arriving rather than anything the panel did.
	await expect(rows(page).first()).toBeVisible();
	let count = 0;
	page.on('request', (request) => {
		const url = request.url();
		if (/\/api\/v1\/sessions\?/.test(url) && !url.includes('count=1')) count++;
	});

	await rows(page).filter({ hasText: 'session-77' }).getByRole('link').click();
	await expect(page.getByRole('dialog').getByText('support-chat')).toBeVisible();
	await page.getByRole('dialog').locator('tbody tr').first().getByRole('link').click();
	await expect(page.getByRole('dialog').getByRole('treeitem').first()).toBeVisible();

	expect(count).toBe(0);
});

test('the listing is still there under the panel', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop', 'the panel covers a phone by design (#12)');
	await signIn(page);
	await rows(page).first().getByRole('link').click();

	await expect(page.getByRole('dialog')).toBeVisible();
	// Not merely mounted: reachable, which is the whole point of #4.
	await expect(page.getByRole('button', { name: 'Live' })).toBeVisible();
	const second = rows(page).nth(1);
	await second.getByRole('link').click();
	await expect(second.getByRole('link')).toHaveAttribute('aria-current', 'true');
});

test('the panel covers the viewport on a phone', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile', 'a wide screen keeps the listing beside it');
	await signIn(page);
	await rows(page).first().getByRole('link').click();

	const panel = page.getByRole('dialog');
	await expect(panel).toHaveAttribute('aria-modal', 'true');
	const box = await panel.boundingBox();
	expect(box?.width).toBeCloseTo(375, 0);
});

test('walking the rows leaves one entry behind, not one per row', async ({ page }) => {
	await signIn(page);
	await rows(page).first().getByRole('link').click();
	const first = page.url();
	// Through the panel's own control, which is how a phone walks the rows at
	// all: there, the panel covers the listing (#12).
	const panel = page.getByRole('dialog');
	await panel.getByRole('button', { name: 'Next row' }).click();
	await panel.getByRole('button', { name: 'Next row' }).click();
	expect(page.url()).not.toBe(first);

	// One gesture back to the listing, however long the scan was (#6).
	await page.goBack();
	await expect(page).toHaveURL(/\/traces$/);
	await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('the next control moves to the next row on screen', async ({ page }) => {
	await signIn(page);
	await rows(page).first().getByRole('link').click();
	const panel = page.getByRole('dialog');
	// The first row has nothing above it, and the control says so rather than
	// disappearing.
	await expect(panel.getByRole('button', { name: 'Previous row' })).toBeDisabled();

	await panel.getByRole('button', { name: 'Next row' }).click();
	await expect(rows(page).nth(1).getByRole('link')).toHaveAttribute('aria-current', 'true');
	await expect(panel.getByRole('button', { name: 'Previous row' })).toBeEnabled();
});

test('a value in a row can still be selected and copied', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop', 'dragging a selection needs a mouse');
	await signIn(page);
	const cell = rows(page).filter({ hasText: 'summarise-release-notes' }).locator('td').nth(1);
	const box = await cell.boundingBox();
	if (!box) throw new Error('the name cell is not on screen');

	// The gesture the row used to swallow: a drag across a cell (spec 008 #15).
	await page.mouse.move(box.x + 2, box.y + box.height / 2);
	await page.mouse.down();
	await page.mouse.move(box.x + box.width - 2, box.y + box.height / 2, { steps: 8 });
	await page.mouse.up();

	expect(await page.evaluate(() => window.getSelection()?.toString() ?? '')).toContain('summarise');
	// And selecting is not opening: nothing was peeked by the drag.
	await expect(page).toHaveURL(/\/traces$/);
	await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('j and k walk the rows, and stop where the listing does', async ({ page }) => {
	await signIn(page);
	await rows(page).first().getByRole('link').click();
	const panel = page.getByRole('dialog');

	await page.keyboard.press('j');
	await expect(rows(page).nth(1).getByRole('link')).toHaveAttribute('aria-current', 'true');
	await page.keyboard.press('k');
	await expect(rows(page).first().getByRole('link')).toHaveAttribute('aria-current', 'true');

	// The top of the listing: the key is dimmed rather than gone, and pressing
	// it again does nothing (spec 008 #13).
	await expect(panel.getByRole('button', { name: 'Previous row' })).toBeDisabled();
	await page.keyboard.press('k');
	await expect(rows(page).first().getByRole('link')).toHaveAttribute('aria-current', 'true');
});

test('a letter typed into a filter stays a letter', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop', 'the panel covers the filter bar on a phone');
	await signIn(page);
	await page.goto('/sessions');
	await rows(page).first().getByRole('link').click();
	const opened = page.url();

	const field = page.getByPlaceholder('Environment');
	await field.click();
	await page.keyboard.type('jk');

	await expect(field).toHaveValue('jk');
	expect(page.url()).toBe(opened);
});

test('the panel expands to the page it points at, selection included', async ({ page }) => {
	await signIn(page);
	await rows(page).filter({ hasText: 'summarise-release-notes' }).getByRole('link').click();
	const panel = page.getByRole('dialog');

	// Pick a node so the link has a selection to carry.
	await panel.getByRole('treeitem').nth(1).click();
	await expect(page).toHaveURL(/obs=[0-9a-f]{16}/);
	const observation = new URL(page.url()).searchParams.get('obs');

	await panel.getByRole('link', { name: 'Open this trace as a page' }).click();
	await expect(page).toHaveURL(new RegExp(`/traces/[0-9a-f]{32}\\?obs=${observation}$`));
	await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('Escape closes the panel and the URL forgets it', async ({ page }) => {
	await signIn(page);
	await rows(page).first().getByRole('link').click();
	await expect(page.getByRole('dialog')).toBeVisible();

	await page.keyboard.press('Escape');
	await expect(page.getByRole('dialog')).toHaveCount(0);
	await expect(page).toHaveURL(/\/traces$/);
});

test('a reload comes back to the panel it was showing', async ({ page }) => {
	await signIn(page);
	await rows(page).first().getByRole('link').click();
	const deep = page.url();

	await page.reload();
	await expect(page).toHaveURL(deep);
	await expect(page.getByRole('dialog').getByRole('treeitem').first()).toBeVisible();
});

test('a session drills one level into a trace and back', async ({ page }) => {
	await signIn(page);
	await page.goto('/sessions');
	await rows(page).filter({ hasText: 'session-77' }).getByRole('link').click();

	const panel = page.getByRole('dialog');
	await expect(page).toHaveURL(/\/sessions\?peek=session-77$/);
	await expect(panel.getByText('support-chat')).toBeVisible();

	// The session's own table, one level down, in the same panel (#9).
	await panel.locator('tbody tr').first().getByRole('link').click();
	await expect(page).toHaveURL(/peek=session-77&trace=[0-9a-f]{32}$/);
	await expect(panel.getByRole('treeitem').first()).toBeVisible();

	// A reload lands on the trace layer, not on the session it came from.
	await page.reload();
	await expect(panel.getByRole('treeitem').first()).toBeVisible();

	// Coming back up costs nothing: the session's own listing was hidden
	// under the trace, not thrown away (PR #10, second review). The endpoint
	// here is the session itself — `/sessions/{id}` — and not the listing.
	let reads = 0;
	page.on('request', (request) => {
		if (/\/api\/v1\/sessions\/[^?]+/.test(request.url())) reads++;
	});
	await panel.getByRole('button', { name: 'Session' }).click();
	await expect(page).toHaveURL(/\/sessions\?peek=session-77$/);
	await expect(panel.getByText('support-chat')).toBeVisible();
	expect(reads).toBe(0);

	// And the table says which trace was open, which the URL no longer can.
	await expect(panel.locator('a[aria-current="true"]')).toHaveCount(1);
});

test('Escape closes the panel even from a filter field', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop', 'the panel covers the filter bar on a phone');
	await signIn(page);
	await page.goto('/sessions');
	await rows(page).first().getByRole('link').click();
	await expect(page.getByRole('dialog')).toBeVisible();

	// Nothing in the interface reverts a filter field on Escape, so guarding
	// Escape there would leave the key meaning nothing at all.
	await page.getByPlaceholder('Environment').click();
	await page.keyboard.type('prod');
	await page.keyboard.press('Escape');

	await expect(page.getByRole('dialog')).toHaveCount(0);
	await expect(page.getByPlaceholder('Environment')).toHaveValue('prod');
});

test('closing after a walk returns focus to the row on screen', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop', 'a phone has no focus ring to return');
	await signIn(page);
	await rows(page).first().getByRole('link').click();
	const panel = page.getByRole('dialog');

	// Three rows along, the row the reader is looking at is not the one they
	// opened, and that is where closing has to put them back.
	await panel.getByRole('button', { name: 'Next row' }).click();
	await panel.getByRole('button', { name: 'Next row' }).click();
	const lit = await page.locator('a[aria-current="true"]').getAttribute('title');

	await page.keyboard.press('Escape');
	await expect(page.getByRole('dialog')).toHaveCount(0);
	expect(await page.evaluate(() => document.activeElement?.getAttribute('title'))).toBe(lit);
});
