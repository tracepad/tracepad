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

	await panel.getByRole('button', { name: 'Session' }).click();
	await expect(page).toHaveURL(/\/sessions\?peek=session-77$/);
	await expect(panel.getByText('support-chat')).toBeVisible();
});
