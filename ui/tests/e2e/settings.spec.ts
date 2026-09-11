import { expect, test, type Page } from '@playwright/test';
import { createProject, signIn, signInAsOwner, state, type Account } from './harness';

// Settings, end to end (Testing): the dry-run/confirm contract rendered, a key
// minted and revoked, and the Server tab's project lifecycle — which used to
// be the Administration section behind the admin token and is an owner's tab
// since spec 028 #14.
//
// Every test here changes something, so every test gets a project of its own.
// Two Playwright projects run these files against one server, and a suite
// whose tests edit each other's retention is a suite that fails at random.

/** Signs in as the project's own editor and opens its tab. */
async function openProjectTab(page: Page, account: Account) {
	await signIn(page, account);
	await page.goto('/settings/project');
}

test('shortening retention previews what it would delete, then asks for the name', async ({
	page
}) => {
	const own = await createProject('retention');
	await openProjectTab(page, own.account);

	await page.getByLabel('Traces, observations and scores').selectOption('Keep for');
	await page.getByLabel('Days of retention').fill('3');
	await page.getByRole('button', { name: 'Save', exact: true }).click();

	// The counts are the server's, and so is the echo it wants back.
	await expect(page.getByText('This would delete')).toBeVisible();
	await expect(page.getByText('the shorter window takes effect on the next sweep')).toBeVisible();
	const apply = page.getByRole('button', { name: 'Shorten and delete' });
	await expect(apply).toBeDisabled();

	const echo = page.getByRole('textbox', { name: /Type the project name/ });
	await echo.fill('not-the-name');
	await expect(apply).toBeDisabled();

	await echo.fill(own.name);
	await expect(apply).toBeEnabled();
	await apply.click();

	await expect(page.getByText('Retention updated.')).toBeVisible();
	const stored = await fetch(`${state().baseURL}/api/v1/projects/${own.id}`, {
		headers: { Authorization: `Bearer ${own.key}` }
	});
	expect(((await stored.json()) as { retention_days: number }).retention_days).toBe(3);
});

// The statistics keep a window of their own, because they outlive the traces
// they summarize (spec 013 #6). Shortening it destroys history the trace sweep
// spares, so it is confirmed like its siblings.
test('the statistics window is set from Settings and survives the round trip', async ({ page }) => {
	const own = await createProject('statswindow');
	await openProjectTab(page, own.account);

	await page.getByLabel('Statistics history').selectOption('Keep for');
	await page.getByLabel('Days of statistics retention').fill('180');
	await page.getByRole('button', { name: 'Save', exact: true }).click();

	await expect(page.getByText('This would delete')).toBeVisible();
	const echo = page.getByRole('textbox', { name: /Type the project name/ });
	await echo.fill(own.name);
	await page.getByRole('button', { name: 'Shorten and delete' }).click();
	await expect(page.getByText('Retention updated.')).toBeVisible();

	const stored = await fetch(`${state().baseURL}/api/v1/projects/${own.id}`, {
		headers: { Authorization: `Bearer ${own.key}` }
	});
	const project = (await stored.json()) as {
		stats_retention_days: number;
		retention_days: number | null;
	};
	expect(project.stats_retention_days).toBe(180);
	// And it moved nothing else: three windows, three promises.
	expect(project.retention_days).toBeNull();
});

test('editing the window after a preview takes the preview away', async ({ page }) => {
	const own = await createProject('repreview');
	await openProjectTab(page, own.account);

	await page.getByLabel('Traces, observations and scores').selectOption('Keep for');
	await page.getByLabel('Days of retention').fill('30');
	await page.getByRole('button', { name: 'Save', exact: true }).click();
	await expect(page.getByText('This would delete')).toBeVisible();

	// The echo the server asks for is the project's name, so it cannot tell one
	// window from another. Confirming here has to mean confirming the numbers
	// on screen, which means the numbers have to go when the question changes.
	await page.getByLabel('Days of retention').fill('1');

	await expect(page.getByText('This would delete')).toBeHidden();
	await expect(page.getByRole('textbox', { name: /Type the project name/ })).toBeHidden();
	await expect(page.getByRole('button', { name: 'Save', exact: true })).toBeVisible();
});

test('a minted key is shown once, and can then be revoked', async ({ page }) => {
	const own = await createProject('keys');
	await openProjectTab(page, own.account);

	await page.getByRole('button', { name: 'Mint a key pair' }).click();

	// The dialog is the same artifact first run prints: both formats, with the
	// secret in them and a warning that this is the only time (spec 007 #9).
	const dialog = page.getByRole('dialog');
	await expect(dialog).toContainText('This is the only time the secret key is shown.');
	await expect(dialog).toContainText('OTEL_EXPORTER_OTLP_TRACES_ENDPOINT');
	await expect(dialog).toContainText('LANGFUSE_SECRET_KEY=tp-sk-');
	await dialog.getByRole('button', { name: 'I have copied it' }).click();

	// Two pairs now; revoking the newer one is not the last-key case, so the
	// server does it on the first request and the card says so.
	const rows = page.getByRole('listitem').filter({ hasText: 'tp-pk-' });
	await expect(rows).toHaveCount(2);
	await rows.last().getByRole('button', { name: 'Revoke' }).click();
	await page.getByRole('button', { name: 'Revoke', exact: true }).last().click();

	await expect(page.getByText(/Key tp-pk-\w+ revoked\./)).toBeVisible();
	await expect(rows).toHaveCount(1);
});

test("erasing a user's data previews it and echoes the user id", async ({ page }) => {
	const own = await createProject('erasure');
	await openProjectTab(page, own.account);

	await page.getByLabel('User id').fill('nobody-here');
	await page.getByRole('button', { name: 'Show what would go' }).click();

	// An empty project is still an honest preview: nothing to remove.
	await expect(page.getByText('Nothing — there is no data to remove.')).toBeVisible();
	const erase = page.getByRole('button', { name: "Erase this user's data" });
	await expect(erase).toBeDisabled();

	await page.getByRole('textbox', { name: /Type the user id/ }).fill('nobody-here');
	await expect(erase).toBeEnabled();
	await erase.click();
	await expect(page.getByText(/Erased 0 traces/)).toBeVisible();
});

// Renaming moved from the admin token to `owner` for the reason it needed the
// token in the first place (spec 028 #3): a project's name is the echo every
// destructive confirmation is typed against.
test('renaming is an editor’s to read and an owner’s to do', async ({ page }) => {
	const own = await createProject('rename');
	await openProjectTab(page, own.account);

	await expect(page.getByLabel('Name', { exact: true })).toBeDisabled();
	await expect(page.getByText("Renaming a project is an owner's")).toBeVisible();
	await expect(page.getByRole('button', { name: 'Rename', exact: true })).toHaveCount(0);

	await signInAsOwner(page, own.id);
	await page.goto('/settings/project');
	await page.getByLabel('Name', { exact: true }).fill(`${own.name}-renamed`);
	await page.getByRole('button', { name: 'Rename' }).click();

	await expect(page.getByText('Renamed.')).toBeVisible();
	// The name rides in `me.projects`, so the sidebar reads the new one too.
	await expect(page.getByText(`${own.name}-renamed`).first()).toBeVisible();
});

test('the Server tab creates, deletes with the echo, and restores', async ({ page }) => {
	const own = await createProject('lifecycle');
	await signInAsOwner(page, own.id);
	await page.goto('/settings/server');

	// The dialog the switcher shares (spec 029 #7): a name, then the keys.
	const name = `disposable-${Math.random().toString(36).slice(2, 8)}`;
	await page.getByRole('button', { name: 'New project' }).click();
	const form = page.getByRole('dialog', { name: 'New project' });
	await form.getByLabel('Name').fill(name);
	await form.getByRole('button', { name: 'Create' }).click();

	// Its first pair, shown once, exactly as minting one is.
	const dialog = page.getByRole('dialog', { name: 'Your new key pair' });
	await expect(dialog).toContainText('LANGFUSE_SECRET_KEY=tp-sk-');
	await dialog.getByRole('button', { name: 'I have copied it' }).click();

	// Made from the Server tab, the owner is still on it (spec 029 #11).
	await expect(page).toHaveURL(/\/settings\/server$/);
	const row = page.getByRole('row').filter({ hasText: name });
	await expect(row).toContainText('Live');

	await row.getByRole('button', { name: 'Delete', exact: true }).click();
	await page.getByRole('button', { name: 'Show what it holds' }).click();
	await expect(page.getByText('the keys stop working immediately')).toBeVisible();

	const remove = page.getByRole('button', { name: 'Delete the project' });
	await expect(remove).toBeDisabled();
	await page.getByRole('textbox', { name: /Type the project name/ }).fill(name);
	await remove.click();

	// Soft-deleted, listed with its purge date, and restorable from there.
	await expect(page.getByText(`${name} is deleted`)).toBeVisible();
	await expect(row).toContainText('Deleted, purged');
	await row.getByRole('button', { name: 'Restore' }).click();
	await expect(page.getByText(`${name} is restored.`)).toBeVisible();
	await expect(row).toContainText('Live');
});

// The tab is absent for a member and the route redirects, which is the two
// halves of the same rule (spec 028 #14).
test('the Server tab belongs to owners', async ({ page }) => {
	const own = await createProject('servertab');
	await openProjectTab(page, own.account);

	await expect(page.getByRole('tab', { name: 'Project' })).toBeVisible();
	await expect(page.getByRole('tab', { name: 'Account' })).toBeVisible();
	await expect(page.getByRole('tab', { name: 'Server' })).toHaveCount(0);

	await page.goto('/settings/server');
	await expect(page).toHaveURL(/\/settings\/project$/);
});

test('a member changes their own name and password', async ({ page }) => {
	const own = await createProject('ownaccount');
	await signIn(page, own.account);
	await page.goto('/settings/account');

	await page.getByLabel('Display name').fill('Renamed Person');
	await page.getByRole('button', { name: 'Save' }).click();
	await expect(page.getByText('Saved.')).toBeVisible();
	// The account menu reads the display name from the same `me`.
	await expect(page.getByRole('button', { name: 'Signed in as Renamed Person' })).toBeVisible();

	// This browser is the only session, and it says which one it is.
	await expect(page.getByText('this browser')).toBeVisible();

	await page.getByLabel('Current password').fill(own.account.password);
	await page.getByLabel('New password', { exact: true }).fill('a-second-password');
	await page.getByLabel('New password again').fill('a-second-password');
	await page.getByRole('button', { name: 'Change the password' }).click();

	await expect(page.getByText('Every other browser was signed out.')).toBeVisible();
});

test('a wrong current password is refused in the server’s words', async ({ page }) => {
	const own = await createProject('badpassword');
	await signIn(page, own.account);
	await page.goto('/settings/account');

	await page.getByLabel('Current password').fill('not-the-password');
	await page.getByLabel('New password', { exact: true }).fill('a-long-enough-one');
	await page.getByLabel('New password again').fill('a-long-enough-one');
	await page.getByRole('button', { name: 'Change the password' }).click();

	await expect(page.getByRole('alert')).toContainText('wrong current password');
});
