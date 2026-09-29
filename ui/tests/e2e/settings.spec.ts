import { expect, test, type Page } from '@playwright/test';
import {
	BCRYPT_WAIT,
	clipped,
	foldsAt,
	createProject,
	fromOwnAddress,
	inviteNobody,
	section,
	sideways,
	signIn,
	signInAsOwner,
	state,
	type Account
} from './harness';

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

	await page.getByLabel('Which program will hold the new key').fill('e2e worker');
	await page.getByRole('button', { name: 'Mint a key pair' }).click();

	// The dialog is the same artifact first run prints: both formats, with the
	// secret in them and a warning that this is the only time (spec 007 #9).
	const dialog = page.getByRole('dialog');
	await expect(dialog).toContainText('This is the only time the secret key is shown.');
	await expect(dialog).toContainText('OTEL_EXPORTER_OTLP_TRACES_ENDPOINT');
	await expect(dialog).toContainText('LANGFUSE_SECRET_KEY=tp-sk-');
	await dialog.getByRole('button', { name: 'I have copied it' }).click();

	// Two pairs now, each saying who minted it and whether it has been used
	// (spec 045 #14): the project's first by the admin token that created it,
	// the new one by the editor signed in here, under the name typed.
	const rows = page.getByRole('row').filter({ hasText: 'tp-pk-' });
	await expect(rows).toHaveCount(2);
	await expect(rows.first()).toContainText('by the admin token');
	await expect(rows.last()).toContainText('e2e worker');
	await expect(rows.last()).toContainText(`by ${own.account.email} (editor)`);
	await expect(rows.last()).toContainText('never');

	// The project's own key is not one of the credentials that see this
	// (spec 045 #4).
	const byKey = await page.request.get(`/api/v1/projects/${own.id}/keys`, {
		headers: { Authorization: `Bearer ${own.key}` }
	});
	expect(byKey.status()).toBe(403);

	// Revoking the newer one is not the last-key case, so the server does it
	// on the first request and the card says so.
	await rows.last().getByRole('button', { name: 'Revoke' }).click();
	// The confirmation is a card under the table. Until it renders, the last
	// "Revoke" on the page is the row's own — and pressing that again closes
	// the card this was meant to confirm. Wait for the card, then press its.
	await expect(page.getByRole('heading', { name: /^Revoke tp-pk-/ })).toBeVisible();
	await page.getByRole('button', { name: 'Revoke', exact: true }).last().click();

	await expect(page.getByText(/Key tp-pk-\w+ revoked\./)).toBeVisible();
	await expect(rows).toHaveCount(1);
});

// What the mint form's default means on the wire (spec 045 #14, Testing #15):
// the key an owner mints without touching the scopes sends spans and reads
// nothing, and the server — not the page — is what says so.
test('an ingest key minted in Settings sends a span and cannot list traces', async ({ page }) => {
	const own = await createProject('ingestkey');
	await signInAsOwner(page, own.id);
	await page.goto('/settings/project');

	await expect(page.getByRole('checkbox', { name: /^ingest/ })).toBeChecked();
	await page.getByLabel('Which program will hold the new key').fill('production app');
	await page.getByRole('button', { name: 'Mint a key pair' }).click();

	const dialog = page.getByRole('dialog', { name: 'Your new key pair' });
	await expect(dialog).toContainText('OTEL_EXPORTER_OTLP_HEADERS');
	const secret = /TRACEPAD_API_KEY=(tp-sk-\S+)/.exec((await dialog.textContent()) ?? '')?.[1];
	expect(secret).toBeTruthy();
	await dialog.getByRole('button', { name: 'I have copied it' }).click();
	await expect(page.getByRole('row').filter({ hasText: 'production app' })).toContainText('ingest');

	const { baseURL } = state();
	const at = (BigInt(Date.now()) - 60_000n) * 1_000_000n;
	const span = await fetch(`${baseURL}/v1/traces`, {
		method: 'POST',
		headers: { Authorization: `Bearer ${secret}`, 'Content-Type': 'application/json' },
		body: JSON.stringify({
			resourceSpans: [
				{
					resource: { attributes: [] },
					scopeSpans: [
						{
							spans: [
								{
									traceId: 'a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5',
									spanId: 'a5a5a5a5a5a5a5a5',
									name: 'ingest-key',
									startTimeUnixNano: String(at),
									endTimeUnixNano: String(at + 1_000_000n)
								}
							]
						}
					]
				}
			]
		})
	});
	expect(span.status).toBe(200);

	const listing = await fetch(`${baseURL}/api/v1/traces`, {
		headers: { Authorization: `Bearer ${secret}` }
	});
	expect(listing.status).toBe(403);
	expect(((await listing.json()) as { error: string }).error).toContain('needs read');
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

	// Made from the Server tab, the owner is on the same tab of the new
	// project (spec 029 #15): the switcher names it, and the server's table —
	// the same one — lists it.
	await expect(page).toHaveURL(/\/p\/[0-9a-f]{32}\/settings\/server$/);
	await expect(page).not.toHaveURL(new RegExp(`/p/${own.id}/`));
	await expect(page.getByRole('button', { name: 'Switch project' })).toHaveText(name);
	const row = page.getByRole('row').filter({ hasText: name });
	await expect(row).toContainText('Live');

	await row.getByRole('button', { name: /^Delete\b/ }).click();
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

// Spec 006 #24: a settings table folds in a box narrower than itself. On a
// phone the keys are the key and Revoke, named for it, and the projects the
// name and the verbs, named for it; on a tablet the boxes are 544 px, narrower
// than either table, so both fold there too.
test('the keys and the projects fold where their box is narrower than the table', async ({
	page
}, testInfo) => {
	// With names longer than any column: a project's and a key's, in one word.
	const own = await createProject(`fold${'x'.repeat(40)}`);
	const phone = testInfo.project.name === 'mobile';
	if (!phone) await page.setViewportSize({ width: 820, height: 1180 });
	await signInAsOwner(page, own.id);

	await page.goto('/settings/project');
	await page.getByLabel('Which program will hold the new key').fill('k'.repeat(60));
	await page.getByRole('button', { name: 'Mint a key pair' }).click();
	await page.getByRole('dialog').getByRole('button', { name: 'I have copied it' }).click();
	const keys = page.getByRole('table').filter({ hasText: 'tp-pk-' });
	await expect(keys.locator('tbody tr')).toHaveCount(2);
	await expect(keys.locator('thead th')).toHaveText(['Name', 'Actions']);
	await expect(keys.locator('tbody tr').first()).toContainText(
		/ingest.* · created .* · by the admin token · last used /
	);
	await expect(keys.getByRole('button', { name: /^Revoke tp-pk-/ })).toHaveCount(2);
	expect(await sideways(keys)).toBeLessThanOrEqual(0);
	expect(await clipped(keys)).toEqual([]);

	await page.goto('/settings/server');
	const projects = page.getByRole('table').filter({ hasText: own.id });
	const row = projects.locator('tbody tr').filter({ hasText: own.id });
	await expect(projects.locator('thead th')).toHaveText(['Name', 'Actions']);
	await expect(row).toContainText('Keep forever · Live');
	await expect(row.getByRole('button', { name: `Settings of ${own.name}` })).toBeVisible();
	await expect(row.getByRole('button', { name: `Delete ${own.name}` })).toBeVisible();
	expect(await sideways(projects)).toBeLessThanOrEqual(0);
	expect(await clipped(projects)).toEqual([]);
});

// Spec 006 #24: on a desktop the keys have their five columns from 640 px and
// the projects theirs from 704, and fold under them as the window narrows. A
// settings card puts 68 px of padding, border and gutter between the window
// and its table.
test('the keys and the projects fold at their own widths on a desktop', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name === 'mobile', 'a desktop window is the test');
	const own = await createProject('foldwidths');
	await signInAsOwner(page, own.id);

	await page.goto('/settings/project');
	const keys = page.getByRole('table').filter({ hasText: 'tp-pk-' });
	await expect(keys.locator('tbody tr')).toHaveCount(1);
	await foldsAt(page, keys, 640, 5, 68, 64);

	await page.goto('/settings/server');
	const projects = page.getByRole('table').filter({ hasText: own.id });
	await expect(projects.locator('tbody tr').first()).toBeVisible();
	await foldsAt(page, projects, 704, 5, 68);
	// The id is what an operator copies into the CLI: on one line, whole.
	const lines = await projects.getByText(own.id).first().evaluate((node) => {
		const range = document.createRange();
		range.selectNodeContents(node);
		return new Set([...range.getClientRects()].map((rect) => Math.round(rect.top))).size;
	});
	expect(lines).toBe(1);
});

// The project on screen is the one being deleted: `me` drops it at once, but
// the tab — and the Restore on it — stays until the next navigation
// (spec 029 #4, edge cases). Pulling the screen away mid-action would leave
// an owner's only project with nowhere to restore it from.
test('deleting the project on screen leaves the tab, and Restore, in place', async ({ page }) => {
	const own = await createProject('onscreen');
	await signInAsOwner(page, own.id);
	await page.goto('/settings/server');
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/settings/server$`));

	// By id: the accounts table on the same tab has a row for the project's
	// editor, whose email carries the name.
	const row = page.getByRole('row').filter({ hasText: own.id });
	await row.getByRole('button', { name: /^Delete\b/ }).click();
	await page.getByRole('button', { name: 'Show what it holds' }).click();
	await page.getByRole('textbox', { name: /Type the project name/ }).fill(own.name);
	await page.getByRole('button', { name: 'Delete the project' }).click();

	await expect(page.getByText(`${own.name} is deleted`)).toBeVisible();
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/settings/server$`));
	await expect(page.getByText('This project is not yours to see')).toHaveCount(0);
	await row.getByRole('button', { name: 'Restore' }).click();
	await expect(page.getByText(`${own.name} is restored.`)).toBeVisible();
	await expect(row).toContainText('Live');
});

// The table is the way into a project's settings (spec 028 #25): a row leads
// to that project's Project tab — a switch of project, so the sidebar names
// it — and the Project tab leads an owner back. A member has no table to go
// back to, and no line saying so.
test('the projects table leads into a project, and the Project tab leads back', async ({
	page
}) => {
	const own = await createProject('drillfrom');
	const other = await createProject('drillto');
	await signInAsOwner(page, own.id);
	await page.goto('/settings/server');
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/settings/server$`));

	const row = page.getByRole('row').filter({ hasText: other.id });
	await row.getByRole('button', { name: 'Settings' }).click();
	await expect(page).toHaveURL(new RegExp(`/p/${other.id}/settings/project$`));
	await expect(page.getByRole('button', { name: 'Switch project' })).toHaveText(other.name);
	await expect(page.getByLabel('Name')).toHaveValue(other.name);

	await page.getByRole('link', { name: 'All projects' }).click();
	await expect(page).toHaveURL(new RegExp(`/p/${other.id}/settings/server$`));

	// The name is a link to the same place, for the middle click and the copy.
	await page.getByRole('row').filter({ hasText: own.id }).getByRole('link', { name: own.name }).click();
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/settings/project$`));
});

// The table is the server's list and the way in is gated by `me`, which the
// shell read at sign-in: a project made meanwhile — the CLI, another owner —
// is on the table and not in the shell, so the tab reads `me` again rather
// than leading to the not-there screen.
test('a project made elsewhere is a way in too', async ({ page }) => {
	const own = await createProject('meanwhile');
	await signInAsOwner(page, own.id);
	// Made after the sign-in, and reached without a reload: the sidebar, the
	// tab — every step a navigation inside the shell.
	const other = await createProject('outofband');
	await (await section(page, 'Settings')).click();
	await page.getByRole('tab', { name: 'Server' }).click();
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/settings/server$`));

	const row = page.getByRole('row').filter({ hasText: other.id });
	await row.getByRole('button', { name: 'Settings' }).click();
	await expect(page).toHaveURL(new RegExp(`/p/${other.id}/settings/project$`));
	await expect(page.getByText('This project is not yours to see')).toHaveCount(0);
	await expect(page.getByLabel('Name')).toHaveValue(other.name);
});

test('a member has no way back to a table they cannot see', async ({ page }) => {
	const own = await createProject('noback');
	await openProjectTab(page, own.account);

	await expect(page.getByLabel('Name')).toHaveValue(own.name);
	await expect(page.getByRole('link', { name: 'All projects' })).toHaveCount(0);
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
	// A sign-in and a password change are two bcrypt waits of up to
	// BCRYPT_WAIT each, which a loaded machine can stretch past the default
	// thirty seconds a test gets; slow() gives this one three times that.
	test.slow();
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

	await expect(page.getByText('Every other browser was signed out.')).toBeVisible({ timeout: BCRYPT_WAIT });
});

// The Account tab is about the person and lives bare (spec 029 #14): a member
// of nothing, whose every bare path is `/p`, still opens it from the menu and
// still owns the password there.
test('an account with no projects opens the Account tab from the menu and changes its password', async ({
	page
}) => {
	// A sign-in and a password change are two bcrypt waits of up to
	// BCRYPT_WAIT each, which a loaded machine can stretch past the default
	// thirty seconds a test gets; slow() gives this one three times that.
	test.slow();
	const nobody = await inviteNobody(state().baseURL, 'nobody');
	await fromOwnAddress(page);
	await page.goto('/login');
	await page.getByLabel('Email').fill(nobody.email);
	await page.getByLabel('Password', { exact: true }).fill(nobody.password);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).toHaveURL(/\/p$/, { timeout: BCRYPT_WAIT });
	await expect(page.getByText('No projects yet')).toBeVisible();

	await page.getByRole('button', { name: /^Signed in as/ }).click();
	await page.getByRole('menuitem', { name: 'Account' }).click();
	await expect(page).toHaveURL(/\/settings\/account$/);
	// The one tab there is: nothing to point Project or Server at.
	await expect(page.getByRole('tab')).toHaveText(['Account']);

	await page.getByLabel('Current password').fill(nobody.password);
	await page.getByLabel('New password', { exact: true }).fill('a-password-of-my-own');
	await page.getByLabel('New password again').fill('a-password-of-my-own');
	await page.getByRole('button', { name: 'Change the password' }).click();
	await expect(page.getByText('Every other browser was signed out.')).toBeVisible({ timeout: BCRYPT_WAIT });
});

// Under a prefix the tab redirects to its bare address, and from there the
// Project tab is a way back under the same id.
test('the Account tab lands bare from under a project, and Project leads back under it', async ({
	page
}) => {
	const own = await createProject('accounttab');
	await signIn(page, own.account);

	await page.goto(`/p/${own.id}/settings/account?x=1`);
	await expect(page).toHaveURL(/\/settings\/account\?x=1$/);
	await expect(page.getByLabel('Display name')).toBeVisible();
	await expect(page.getByRole('tab')).toHaveText(['Project', 'Account']);
	// The sidebar is the shell's; the screen is under no project, and the
	// switcher says so the way it does on `/p`.
	await expect(page.getByRole('button', { name: 'Switch project' })).toHaveText(
		'Choose a project'
	);

	await page.getByRole('tab', { name: 'Project' }).click();
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/settings/project$`));
	await page.getByRole('tab', { name: 'Account' }).click();
	await expect(page).toHaveURL(/\/settings\/account$/);
});

test('a wrong current password is refused in the server’s words', async ({ page }) => {
	// A sign-in and a password change are two bcrypt waits of up to
	// BCRYPT_WAIT each, which a loaded machine can stretch past the default
	// thirty seconds a test gets; slow() gives this one three times that.
	test.slow();
	const own = await createProject('badpassword');
	await signIn(page, own.account);
	await page.goto('/settings/account');

	await page.getByLabel('Current password').fill('not-the-password');
	await page.getByLabel('New password', { exact: true }).fill('a-long-enough-one');
	await page.getByLabel('New password again').fill('a-long-enough-one');
	await page.getByRole('button', { name: 'Change the password' }).click();

	await expect(page.getByRole('alert')).toContainText('wrong current password', { timeout: BCRYPT_WAIT });
});
