import { expect, test, type Page } from '@playwright/test';
import { createProject, state } from './harness';

// The Prompts screens (spec 021, Testing — e2e) against the real binary. The
// corpus is seeded through the API in a project of its own (spec 016 #18),
// because this suite *deletes* a prompt and ends with an empty listing, which
// is not a thing to do to a project another suite is reading.
//
// Serial, and deliberately so: the scenario is one story — publish, read, diff,
// append, promote, delete — and each step is the state the next one starts
// from. Both Playwright projects run this file, each in its own worker and so
// in its own project.

test.describe.configure({ mode: 'serial' });

const CHAT = 'support-answer';
const TEXT = 'one-liner';

/** The suite's own project, minted once per worker. */
let own: Promise<{ key: string }> | null = null;
const project = () => (own ??= createProject('prompts'));

async function signIn(page: Page) {
	const { key } = await project();
	await page.goto('/login');
	await page.getByLabel('Project key').fill(key);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).toHaveURL(/\/traces$/);
}

async function call(method: string, path: string, body?: unknown) {
	const { baseURL } = state();
	const { key } = await project();
	return fetch(`${baseURL}${path}`, {
		method,
		headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
		body: body === undefined ? undefined : JSON.stringify(body)
	});
}

let seeded: Promise<void> | null = null;

/** Two versions of a chat prompt, `production` left on the first. */
function seed(): Promise<void> {
	seeded ??= (async () => {
		const versions = [
			{
				type: 'chat',
				prompt: [{ role: 'system', content: 'You are terse.' }],
				config: { model: 'claude-sonnet-5', temperature: 0.2 },
				commit_message: 'first cut',
				labels: ['production']
			},
			{
				prompt: [{ role: 'system', content: 'You are terse and cite the source.' }],
				commit_message: 'cite the source'
			}
		];
		for (const version of versions) {
			const posted = await call('POST', `/api/v1/prompts/${CHAT}/versions`, version);
			if (!posted.ok) throw new Error(`push ${CHAT}: ${posted.status} ${await posted.text()}`);
		}
	})();
	return seeded;
}

test.beforeEach(async () => {
	await seed();
});

test('the sidebar leads to the listing, and the row carries its label', async ({ page }) => {
	await signIn(page);
	const nav = page.getByRole('navigation', { name: 'Sections' });
	await nav.getByRole('link', { name: 'Prompts' }).click();

	await expect(page).toHaveURL(/\/prompts$/);
	await expect(nav.getByRole('link', { name: 'Prompts' })).toHaveAttribute('aria-current', 'page');
	const row = page.locator('tbody tr').filter({ hasText: CHAT });
	await expect(row).toContainText('chat');
	await expect(row).toContainText('v2');
	// The chip says where production points, which is still the first version.
	await expect(row.getByText('production')).toBeVisible();
	await expect(row).toContainText('v1');
});

test('the prompt page opens on the latest version with its list beside it', async ({ page }) => {
	await signIn(page);
	await page.goto('/prompts');
	await page.getByRole('link', { name: CHAT }).click();

	await expect(page).toHaveURL(new RegExp(`/prompts/${CHAT}$`));
	await expect(page.getByRole('heading', { name: CHAT })).toBeVisible();
	// The body of v2, whole. It carries no config, so no config is shown.
	await expect(page.getByRole('heading', { name: 'v2' })).toBeVisible();
	await expect(page.getByText('You are terse and cite the source.')).toBeVisible();
	await expect(page.getByLabel('Config', { exact: true })).toHaveCount(0);
	// Both versions in the list; a click reads the older one, config and all.
	const versions = page.getByRole('table', { name: 'Versions' });
	await expect(versions.locator('tbody tr')).toHaveCount(2);
	await versions.getByRole('link', { name: /v1/ }).click();
	await expect(page).toHaveURL(/version=1/);
	await expect(page.getByText('You are terse.', { exact: true })).toBeVisible();
	await expect(page.getByLabel('Config', { exact: true })).toContainText('claude-sonnet-5');
	// The two links that answer "where did this run" (#2).
	await expect(page.getByRole('link', { name: 'Traces with v1' })).toHaveAttribute(
		'href',
		`/traces?prompt=${CHAT}%401`
	);
});

test('the diff is the server-rendered patch, painted', async ({ page }) => {
	await signIn(page);
	await page.goto(`/prompts/${CHAT}?diff=1..2`);

	const diff = page.getByLabel('Diff of v1 and v2');
	await expect(diff.locator('[data-kind="remove"]').first()).toContainText('You are terse.');
	await expect(diff.locator('[data-kind="add"]').first()).toContainText('cite the source');
	await expect(diff.locator('[data-kind="hunk"]').first()).toBeVisible();
	// A file header starts with --- and +++ and is neither (#3).
	await expect(diff.locator('[data-kind="file"]').first()).toContainText('prompt (version 1)');

	// A version against itself is identical, not empty (edge cases).
	await page.getByLabel('Diff from version').fill('2');
	await page.getByLabel('Diff from version').press('Enter');
	await expect(page).toHaveURL(/diff=2\.\.2/);
	await expect(page.getByText(/identical/)).toBeVisible();
});

test('a new version is written from the one on screen', async ({ page }) => {
	await signIn(page);
	await page.goto(`/prompts/${CHAT}?version=2`);
	await page.getByRole('button', { name: 'New version' }).click();

	await expect(page).toHaveURL(new RegExp(`/prompts/${CHAT}/versions/new\\?from=2`));
	const content = page.getByLabel('Content of message 1');
	await expect(content).toHaveValue('You are terse and cite the source.');
	await content.fill('You are terse, cite the source, and never guess.');
	await page.getByLabel('Commit message').fill('never guess');
	await page.getByRole('button', { name: 'Save' }).click();

	// It lands on the version the server assigned (#4).
	await expect(page).toHaveURL(new RegExp(`/prompts/${CHAT}\\?version=3`));
	await expect(page.getByRole('heading', { name: 'v3' })).toBeVisible();
	// Twice: in the version list and on the view it opened.
	await expect(page.getByText('never guess', { exact: true })).toHaveCount(2);
	await expect(page.getByText('You are terse, cite the source, and never guess.')).toBeVisible();
});

test('promoting a label asks first and names the move', async ({ page }) => {
	await signIn(page);
	await page.goto(`/prompts/${CHAT}?version=3`);

	await page.getByRole('button', { name: /Add label/ }).click();
	await page.getByLabel('Label to add').fill('production');
	await page.getByRole('button', { name: 'Add', exact: true }).click();

	// A deploy from a mis-click needs one more click (#6).
	const dialog = page.getByRole('alertdialog');
	await expect(dialog).toContainText('production: v1 → v3');
	await dialog.getByRole('button', { name: 'Move production here' }).click();

	await expect(page.getByRole('button', { name: 'Remove production' })).toBeVisible();
	// And the chips in the header follow it.
	await expect(page.getByRole('link', { name: /production/ }).first()).toHaveAttribute(
		'href',
		/version=3/
	);
});

test('a text prompt is created from the editor', async ({ page }) => {
	await signIn(page);
	await page.goto('/prompts/new');

	const save = page.getByRole('button', { name: 'Save' });
	await expect(save).toBeDisabled();
	await page.getByLabel('Name').fill(TEXT);
	await page.getByLabel('text', { exact: true }).check();
	await page.getByLabel('Prompt', { exact: true }).fill('Summarize {{input}} in one line.');
	await save.click();

	await expect(page).toHaveURL(new RegExp(`/prompts/${TEXT}\\?version=1`));
	await expect(page.getByText('Summarize {{input}} in one line.')).toBeVisible();
});

test('no screen scrolls the page sideways', async ({ page }) => {
	await signIn(page);
	for (const path of [
		'/prompts',
		`/prompts/${CHAT}`,
		`/prompts/${CHAT}?diff=1..3`,
		'/prompts/new',
		`/prompts/${CHAT}/versions/new?from=3`
	]) {
		await page.goto(path);
		await expect(page.getByRole('heading').first()).toBeVisible();
		const overflow = await page.evaluate(() => {
			const root = document.scrollingElement ?? document.documentElement;
			return root.scrollWidth - root.clientWidth;
		});
		expect(overflow, path).toBeLessThanOrEqual(0);
	}
});

test('deleting a prompt shows the dry run, refuses a wrong echo, and empties the listing', async ({
	page
}) => {
	await signIn(page);
	await page.goto(`/prompts/${CHAT}`);
	await page.getByRole('button', { name: 'Delete prompt' }).click();
	await page.getByRole('button', { name: 'Show what would go' }).click();

	// The server's own counts, on screen (spec 005 #8).
	const card = page.getByRole('dialog');
	await expect(card).toContainText('versions');
	await expect(card.getByText('3', { exact: true })).toBeVisible();
	await expect(card).toContainText('labels');

	const echo = card.getByRole('textbox');
	const remove = card.getByRole('button', { name: 'Delete this prompt' });
	await echo.fill('support-answers');
	await expect(remove).toBeDisabled();
	await echo.fill(CHAT);
	await remove.click();

	await expect(page).toHaveURL(/\/prompts$/);
	await expect(page.getByRole('link', { name: CHAT })).toHaveCount(0);

	// The other name goes the same way, and then the empty state is the one
	// that teaches the CLI (#9).
	await page.goto(`/prompts/${TEXT}`);
	await page.getByRole('button', { name: 'Delete prompt' }).click();
	await page.getByRole('button', { name: 'Show what would go' }).click();
	await page.getByRole('dialog').getByRole('textbox').fill(TEXT);
	await page.getByRole('dialog').getByRole('button', { name: 'Delete this prompt' }).click();

	await expect(page.getByRole('heading', { name: 'No prompts yet' })).toBeVisible();
	await expect(page.getByText('tracepad prompts push')).toBeVisible();
});
