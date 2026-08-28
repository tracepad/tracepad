import { expect, test, type Page } from '@playwright/test';
import { state } from './harness';

// Sessions, end to end against the real binary and the endpoint spec 007 added
// (Testing): the listing's aggregates, the session view, and a trace opened
// from it. The corpus carries two sessions and five traces that name none, so
// "aggregated from traces" is testable rather than merely asserted.

async function signIn(page: Page) {
	await page.goto(state().preAuthed);
	await expect(page).toHaveURL(/\/traces$/);
}

test('the listing rolls the corpus up by session', async ({ page }) => {
	await signIn(page);
	await page.getByRole('link', { name: 'Sessions' }).click();

	await expect(page).toHaveURL(/\/sessions$/);
	// Two sessions, and only two: the traces that named none are not sessions
	// of one.
	await expect(page.getByRole('row')).toHaveCount(3);
	const row = page.getByRole('row').filter({ hasText: 'session-77' });
	await expect(row).toContainText('$0.0010');
});

test('a filter narrows the listing and stays in the URL', async ({ page }) => {
	await signIn(page);
	await page.goto('/sessions?environment=staging');

	await expect(page.getByText('session-12')).toBeVisible();
	await expect(page.getByText('session-77')).toHaveCount(0);

	// The environment field is a link somebody can send, not hidden state.
	await expect(page.getByPlaceholder('Environment')).toHaveValue('staging');
});

test('a session opens onto its traces, and a trace onto its tree', async ({ page }) => {
	await signIn(page);
	await page.goto('/sessions');
	// By row rather than by position: the two sessions were last active within
	// milliseconds of each other, and which one sorts first is the server's
	// business.
	await page.getByRole('row').filter({ hasText: 'session-77' }).getByRole('link').click();

	await expect(page).toHaveURL(/\/sessions\/session-77$/);
	// The totals header comes from `GET /api/v1/sessions/{id}`.
	await expect(page.locator('dt').filter({ hasText: /^Traces$/ })).toBeVisible();
	await expect(page.locator('dt').filter({ hasText: /^With errors$/ })).toBeVisible();
	await expect(page.getByText('support-chat')).toBeVisible();

	await page.getByRole('link', { name: /\d/ }).last().click();
	await expect(page).toHaveURL(/\/traces\/[0-9a-f]{32}/);
	await expect(page.getByRole('treeitem').first()).toBeVisible();
});

test('an empty listing explains what a session is', async ({ page }) => {
	await signIn(page);
	// A window nothing falls in: the corpus is fixed in the past.
	await page.goto('/sessions?from=2099-01-01T00:00:00Z');

	await expect(page.getByText('No session matches these filters')).toBeVisible();
	await page.getByRole('button', { name: 'Clear filters' }).click();
	await expect(page.getByText('session-77')).toBeVisible();
});
