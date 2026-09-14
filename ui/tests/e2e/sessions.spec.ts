import { expect, test, type Page } from '@playwright/test';
import { signIn as enter, state } from './harness';

// Sessions, end to end against the real binary and the endpoint spec 007 added
// (Testing): the listing's aggregates, the session view, and a trace opened
// from it. The corpus carries five sessions and traces that name none, so
// "aggregated from traces" is testable rather than merely asserted.

/** The fixture trace of `session-77`, whose header names both ids. */
const SESSION_TRACE = '4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f';

async function signIn(page: Page) {
	await enter(page, state().member);
}

test('the listing rolls the corpus up by session', async ({ page }) => {
	await signIn(page);
	await page.getByRole('link', { name: 'Sessions' }).click();

	await expect(page).toHaveURL(/\/sessions$/);
	// Six sessions, and only six: the traces that named none are not
	// sessions of one. The header is the seventh row.
	await expect(page.getByRole('row')).toHaveCount(7);
	const row = page.getByRole('row').filter({ hasText: 'session-77' });
	await expect(row).toContainText('$0.0010');
});

test('a filter narrows the listing and stays in the URL', async ({ page }) => {
	await signIn(page);
	await page.goto('/sessions?environment=staging');

	await expect(page.getByText('session-12')).toBeVisible();
	await expect(page.getByText('session-77')).toHaveCount(0);

	// The environment control is a link somebody can send, not hidden state:
	// it names what is filtering on its face (spec 027 #8).
	await expect(page.getByRole('button', { name: 'Environment: staging' })).toBeVisible();
});

test('a session page opens onto its traces, and a trace into a panel', async ({ page }) => {
	await signIn(page);
	// The page rather than the listing's panel: a row opens the panel now
	// (spec 008), and this is the full page it points at.
	await page.goto('/sessions/session-77');

	// The totals header comes from `GET /api/v1/sessions/{id}`.
	await expect(page.locator('dt').filter({ hasText: /^Traces$/ })).toBeVisible();
	await expect(page.locator('dt').filter({ hasText: /^With errors$/ })).toBeVisible();
	await expect(page.getByText('support-chat')).toBeVisible();

	// Its trace table is a listing like any other (spec 008 #10). The row's
	// own link is the first one; the user id follows it (spec 023 #14). The
	// session id does not, here — see below.
	await page.locator('tbody tr').last().getByRole('link').first().click();
	await expect(page).toHaveURL(/\/sessions\/session-77\?peek=[0-9a-f]{32}$/);
	await expect(page.getByRole('dialog').getByRole('treeitem').first()).toBeVisible();
});

test('a session id in the traces table opens the session, not the panel', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces');

	// The cell's own link stops the row's click, so the reader lands on the
	// session rather than on a panel over the trace (spec 023 #16).
	await page.getByRole('link', { name: 'session-77', exact: true }).first().click();
	await expect(page).toHaveURL(/\/sessions\/session-77$/);
	await expect(page.getByRole('dialog')).toHaveCount(0);
	await expect(page.locator('dt').filter({ hasText: /^Traces$/ })).toBeVisible();
});

// A table already inside the session it would link to: the cell stays the text
// it was, because a link to where the reader stands is not a destination — and
// from a panel, following it would tear down the listing to arrive at it
// (spec 023 #17).
test('a trace table inside a session does not link to that session', async ({ page }) => {
	await signIn(page);
	await page.goto('/sessions/session-77');

	await expect(page.getByText('support-chat')).toBeVisible();
	const table = page.locator('tbody');
	await expect(table.getByRole('link', { name: 'session-77', exact: true })).toHaveCount(0);
	// Still on screen — text, not a destination.
	await expect(table.getByText('session-77').first()).toBeVisible();

	// The same table in the listing's panel, and the trace drilled out of it:
	// its meta leaves the session out for the same reason.
	await page.goto('/sessions?peek=session-77');
	const panel = page.getByRole('dialog');
	await expect(panel.getByText('support-chat')).toBeVisible();
	await expect(panel.getByRole('link', { name: 'session-77', exact: true })).toHaveCount(0);

	await panel.locator('tbody tr').first().getByRole('link').first().click();
	await expect(panel.getByRole('treeitem').first()).toBeVisible();
	await expect(panel.getByRole('link', { name: 'session-77', exact: true })).toHaveCount(0);
});

// The header and a panel's meta carry the session from the width the meta
// survives at; below it the whole meta is out, so these cases are a desk.
test.describe('at a desk', () => {
	test.use({ viewport: { width: 1280, height: 800 } });

	test("a trace header's session id opens the session", async ({ page }) => {
		await signIn(page);
		await page.goto(`/traces/${SESSION_TRACE}`);

		await page.locator('header').getByRole('link', { name: 'session-77', exact: true }).click();
		await expect(page).toHaveURL(/\/sessions\/session-77$/);
		await expect(page.locator('dt').filter({ hasText: /^Traces$/ })).toBeVisible();
	});

	// The panel is the path spec 008 #3 calls the normal one, so the same
	// destination is in it (spec 023 #17).
	test("a trace panel's session id opens the session", async ({ page }) => {
		await signIn(page);
		await page.goto('/traces');

		await page
			.locator('tbody tr')
			.filter({ hasText: 'session-77' })
			.first()
			.getByRole('link')
			.first()
			.click();
		const panel = page.getByRole('dialog');
		await expect(panel.getByRole('treeitem').first()).toBeVisible();

		await panel.getByRole('link', { name: 'session-77', exact: true }).click();
		await expect(page).toHaveURL(/\/sessions\/session-77$/);
		await expect(page.locator('dt').filter({ hasText: /^Traces$/ })).toBeVisible();
	});
});

test('an empty listing explains what a session is', async ({ page }) => {
	await signIn(page);
	// A window nothing falls in: the corpus is fixed in the past.
	await page.goto('/sessions?from=2099-01-01T00:00:00Z');

	await expect(page.getByText('No session matches these filters')).toBeVisible();
	await page.getByRole('button', { name: 'Clear filters' }).click();
	await expect(page.getByText('session-77')).toBeVisible();
});
