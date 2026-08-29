import { expect, test, type Page } from '@playwright/test';
import { ADMIN_TOKEN, createProject, state } from './harness';

// Settings, end to end (Testing): the dry-run/confirm contract rendered, a key
// minted and revoked, and the Administration section's lifecycle.
//
// Every test here changes something, so every test gets a project of its own.
// Two Playwright projects run these files against one server, and a suite
// whose tests edit each other's retention is a suite that fails at random.

/** Signs in with a key entered by hand, which is how a second project is opened. */
async function signInAs(page: Page, key: string) {
	await page.goto('/login');
	await page.getByLabel('Project key').fill(key);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).toHaveURL(/\/traces$/);
	await page.goto('/settings');
}

async function unlockAdministration(page: Page) {
	await page.getByLabel('Admin token').fill(ADMIN_TOKEN);
	await page.getByRole('button', { name: 'Unlock' }).click();
	await expect(page.getByText('Unlocked on this browser.')).toBeVisible();
}

test('shortening retention previews what it would delete, then asks for the name', async ({
	page
}) => {
	const own = await createProject('retention');
	await signInAs(page, own.key);

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

test('editing the window after a preview takes the preview away', async ({ page }) => {
	const own = await createProject('repreview');
	await signInAs(page, own.key);

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
	await signInAs(page, own.key);

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
	await signInAs(page, own.key);

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

test('renaming says it needs the admin token, and works once it has one', async ({ page }) => {
	const own = await createProject('rename');
	await signInAs(page, own.key);

	// Shown, disabled, with the reason and the CLI equivalent (spec 007 #12).
	await expect(page.getByLabel('Name')).toBeDisabled();
	await expect(page.getByText('Renaming needs the admin token')).toBeVisible();

	await unlockAdministration(page);
	await page.getByLabel('Name').fill(`${own.name}-renamed`);
	await page.getByRole('button', { name: 'Rename' }).click();

	await expect(page.getByText('Renamed.')).toBeVisible();
});

test('administration creates, deletes with the echo, and restores', async ({ page }) => {
	const own = await createProject('lifecycle');
	await signInAs(page, own.key);
	await unlockAdministration(page);

	const name = `disposable-${Math.random().toString(36).slice(2, 8)}`;
	await page.getByLabel('New project').fill(name);
	await page.getByRole('button', { name: 'Create' }).click();

	// Its first pair, shown once, exactly as minting one is.
	const dialog = page.getByRole('dialog');
	await expect(dialog).toContainText('LANGFUSE_SECRET_KEY=tp-sk-');
	await dialog.getByRole('button', { name: 'I have copied it' }).click();

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

test('a wrong admin token is refused without touching the session', async ({ page }) => {
	const own = await createProject('badtoken');
	await signInAs(page, own.key);

	await page.getByLabel('Admin token').fill('not-the-token');
	await page.getByRole('button', { name: 'Unlock' }).click();

	await expect(page.getByRole('alert')).toContainText('not this server');
	// Still signed in: a bad management credential is not a bad project key.
	await page.goto('/traces');
	await expect(page.getByRole('heading', { level: 1, name: 'Traces' })).toBeVisible();
});
