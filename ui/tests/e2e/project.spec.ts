import { expect, test, type Page } from '@playwright/test';
import { createProject, FAILING_TRACE, signIn, signInAsOwner, state } from './harness';

// The project in the URL and the switcher (spec 029, Testing): every screen
// lives under `/p/{id}`, a bare path redirects to the remembered project, a
// switch keeps the section and the filters and drops what was open, an id the
// account cannot reach is a screen rather than a wall of 403s, and an owner
// can make a project from the menu.

const ID = '[0-9a-f]{32}';

/** Opens the switcher and picks the project called `name`. */
async function switchTo(page: Page, name: string) {
	await page.getByRole('button', { name: 'Switch project' }).click();
	const menu = page.getByRole('menu');
	// An owner reaches every project the suite has minted, which is well over
	// eight: the box narrows the list to the one wanted.
	const box = menu.getByRole('searchbox', { name: 'Filter projects' });
	if (await box.isVisible()) await box.fill(name);
	await menu.getByRole('menuitemradio', { name: new RegExp(`^${name}\\b`) }).click();
}

test('the owner lands on the remembered project, and a bare path keeps its filter', async ({
	page
}) => {
	const own = await createProject('bare');
	await signInAsOwner(page, own.id);
	// The project's front page is the dashboard (spec 034 #1), which writes
	// its window into the address.
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/dashboard(\\?|$)`));

	await page.goto('/');
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/dashboard(\\?|$)`));

	await page.goto('/traces?environment=prod');
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/traces\\?environment=prod$`));
	await expect(page.getByText('Environment: prod')).toBeVisible();
});

test('the switcher names the projects with a count, and keeps the section and the filters', async ({
	page
}) => {
	const { project: seeded } = state();
	const own = await createProject('switch');
	await signInAsOwner(page, seeded);

	await page.goto(`/p/${seeded}/traces/${FAILING_TRACE}?environment=prod`);
	await page.getByRole('button', { name: 'Switch project' }).click();
	const menu = page.getByRole('menu');
	const box = menu.getByRole('searchbox', { name: 'Filter projects' });
	if (await box.isVisible()) await box.fill(own.name);
	const row = menu.getByRole('menuitemradio', { name: new RegExp(`^${own.name}\\b`) });
	// The caption is the traffic of the last day (#5), asked for on open (#8).
	await expect(row).toContainText(/(no traces|\d[\d,]* traces?) · 24h/);
	await row.click();

	// The trace is the old project's answer and is dropped; the filter is a
	// question, and travels (#6).
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/traces\\?environment=prod$`));
	await expect(page.getByRole('button', { name: 'Switch project' })).toHaveText(own.name);

	// Settings keeps its tab across a switch (#6) — the two tabs that are
	// about a project; the Account tab is under none (#14).
	await page.goto(`/p/${own.id}/settings/server`);
	const seededName = await seededProjectName();
	await switchTo(page, seededName);
	await expect(page).toHaveURL(new RegExp(`/p/${seeded}/settings/server$`));

	await page.goto('/settings/account');
	await switchTo(page, own.name);
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/dashboard(\\?|$)`));
});

test('an editor of one project sees one row, no New project, and a not-there screen elsewhere', async ({
	page
}) => {
	const { project: seeded } = state();
	const own = await createProject('editor');
	await signIn(page, own.account);
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/dashboard(\\?|$)`));

	await page.getByRole('button', { name: 'Switch project' }).click();
	const menu = page.getByRole('menu');
	await expect(menu.getByRole('menuitemradio')).toHaveCount(1);
	await expect(menu.getByRole('menuitem', { name: 'New project' })).toHaveCount(0);
	await page.keyboard.press('Escape');

	// The seeded project is the owner's, not this editor's: a screen, and no
	// request with that id in the header (#4).
	const named: string[] = [];
	page.on('request', (request) => {
		if (request.headers()['x-tracepad-project'] === seeded) named.push(request.url());
	});
	await page.goto(`/p/${seeded}/traces`);
	await expect(page.getByRole('heading', { name: 'This project is not yours to see' })).toBeVisible();
	// The switcher is open beside it, with the way out.
	await expect(page.getByRole('menu').getByRole('menuitemradio', { name: new RegExp(`^${own.name}\\b`) })).toBeVisible();
	expect(named).toEqual([]);
});

test('New project from the switcher shows the keys once and lands on the new dashboard', async ({
	page
}) => {
	const own = await createProject('maker');
	await signInAsOwner(page, own.id);
	const name = `made-${Math.random().toString(36).slice(2, 8)}`;

	await page.getByRole('button', { name: 'Switch project' }).click();
	await page.getByRole('menuitem', { name: 'New project' }).click();
	const dialog = page.getByRole('dialog', { name: 'New project' });
	await dialog.getByLabel('Name').fill(name);
	await dialog.getByRole('button', { name: 'Create' }).click();

	const keys = page.getByRole('dialog', { name: 'Your new key pair' });
	await expect(keys).toBeVisible();
	await expect(keys.getByText('This is the only time the secret key is shown.')).toBeVisible();
	await keys.getByRole('button', { name: 'I have copied it' }).click();
	await expect(keys).toBeHidden();

	// On its dashboard, where a fresh project's instructions are (spec 034 #6).
	await expect(page).toHaveURL(new RegExp(`/p/${ID}/dashboard`));
	await expect(page).not.toHaveURL(new RegExp(`/p/${own.id}/`));
	await expect(page.getByRole('button', { name: 'Switch project' })).toHaveText(name);
	await expect(page.getByRole('heading', { name: 'No traces yet' })).toBeVisible();
	await expect(page.getByText('OTEL_EXPORTER_OTLP_TRACES_ENDPOINT')).toBeVisible();
});

test('a reload stays put, and the login round trip returns to the prefixed path', async ({
	page
}) => {
	const own = await createProject('reload');
	await signIn(page, own.account);

	await page.goto(`/p/${own.id}/prompts`);
	await page.reload();
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/prompts$`));
	await expect(page.getByRole('heading', { name: 'Prompts', level: 1 })).toBeVisible();

	await page.getByRole('button', { name: /^Signed in as/ }).click();
	await page.getByRole('menuitem', { name: 'Sign out' }).click();
	await expect(page).toHaveURL(/\/login/);

	await page.goto(`/p/${own.id}/prompts?q=x`);
	await expect(page).toHaveURL(/\/login\?next=/);
	await page.getByLabel('Email').fill(own.account.email);
	await page.getByLabel('Password', { exact: true }).fill(own.account.password);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/prompts\\?q=x$`));
});

/** The name of the project first run created, which the corpus is in. */
async function seededProjectName(): Promise<string> {
	const { baseURL, key, project } = state();
	const response = await fetch(`${baseURL}/api/v1/projects/${project}`, {
		headers: { Authorization: `Bearer ${key}` }
	});
	if (!response.ok) throw new Error(`read the seeded project: ${response.status}`);
	return ((await response.json()) as { name: string }).name;
}

// A path that names no screen is a 404 under the prefix as it was without
// one; the bare redirect adds a prefix to a path that has none, not another
// one to a path that already does.
test('a path that is no screen is a 404, prefixed or bare', async ({ page }) => {
	const own = await createProject('nowhere');
	await signIn(page, own.account);

	await page.goto(`/p/${own.id}/nonsense`);
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/nonsense$`));
	await expect(page.getByRole('heading', { name: 'No such page' })).toBeVisible();

	await page.goto('/nonsense');
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/nonsense$`));
	await expect(page.getByRole('heading', { name: 'No such page' })).toBeVisible();
	await page.getByRole('link', { name: 'Back to the dashboard' }).click();
	await expect(page).toHaveURL(new RegExp(`/p/${own.id}/dashboard(\\?|$)`));
});
