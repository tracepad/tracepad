import { expect, test, type Page } from '@playwright/test';
import { clipped, createProject, foldsAt, section, sideways, signIn as enter, state } from './harness';

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
let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('prompts'));

async function signIn(page: Page) {
	await enter(page, (await project()).account);
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

/**
 * The version whose first message carries a custom role, filled in by the
 * scenario that writes it. The sideways check reads the editor of *that*
 * version, and a number written down here instead would go on passing once the
 * tests above it shifted — the editor's failure state renders a heading and
 * overflows nothing, so the widest role row would quietly stop being checked
 * (found in review of PR #45).
 */
let customRole: number | null = null;

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
	await (await section(page, 'Prompts')).click();

	await expect(page).toHaveURL(/\/prompts$/);
	await expect(await section(page, 'Prompts')).toHaveAttribute('aria-current', 'page');
	await page.keyboard.press('Escape');
	const row = page.locator('tbody tr').filter({ hasText: CHAT });
	await expect(row).toContainText('chat');
	await expect(row).toContainText('v2');
	// The chip says where production points, which is still the first version.
	await expect(row.getByText('production')).toBeVisible();
	await expect(row).toContainText('v1');
});

// Spec 006 #22: on a phone a prompt is its name and its labels — which version
// is live — and the type, latest version and date fold under the name.
test('on a phone the prompts fold rather than scroll', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile', 'the narrow width is the test');
	await signIn(page);
	await page.goto('/prompts');

	const table = page.locator('main table');
	await expect(table.locator('thead th')).toHaveText(['Name', 'Labels']);
	await expect(page.getByRole('row').filter({ hasText: CHAT })).toContainText(/chat · v2 · updated/);
	expect(await sideways(table)).toBeLessThanOrEqual(0);
	expect(await clipped(table)).toEqual([]);
});

// Spec 006 #22: on a desktop the five columns are there from 672 px, and not
// after they have been left for a narrower window.
test('the prompts fold at their own width on a desktop', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name === 'mobile', 'a desktop window is the test');
	await signIn(page);
	await page.goto('/prompts');
	await foldsAt(page, page.locator('main table'), 672, 5);
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
	// Under the project on screen (spec 029 #2), like every link.
	await expect(page.getByRole('link', { name: 'Traces with v1' })).toHaveAttribute(
		'href',
		new RegExp(`^/p/[0-9a-f]{32}/traces\\?prompt=${CHAT}%401$`)
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

// Opening a diff is about the diff. The page went on reading the version it
// was reading — which is what *Diff*'s own default, the version list's
// highlight, *New version*'s prefill and *Close diff* all stand on (found in
// review of PR #40).
test('the diff keeps the version being read, and closing comes back to it', async ({ page }) => {
	await signIn(page);
	await page.goto(`/prompts/${CHAT}?version=1`);
	await page.getByRole('button', { name: 'Diff' }).click();

	// On v1 there is no version before it, so the pair is v1 against the one
	// after it rather than a comparison of v1 with itself (#40 review).
	await expect(page).toHaveURL(/version=1/);
	await expect(page).toHaveURL(/diff=1\.\.2/);
	await expect(page.getByText(/identical/)).toHaveCount(0);
	await page.getByLabel('Diff to version').fill('3');
	await page.getByLabel('Diff to version').press('Enter');
	await expect(page).toHaveURL(/version=1/);
	await expect(page).toHaveURL(/diff=1\.\.3/);

	await page.getByRole('button', { name: 'Close the diff' }).click();
	await expect(page).toHaveURL(new RegExp(`/prompts/${CHAT}\\?version=1$`));
	await expect(page.getByRole('heading', { name: 'v1' })).toBeVisible();
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

// The role is a select of the roles the runtimes name, and an added message
// alternates (spec 021 #16). The datalist this replaces was unusable for the
// one thing it existed for: a browser filters its suggestions by the text
// already in the field, and the field is never empty.
test('a role is picked from the select, and an added message alternates', async ({ page }) => {
	await signIn(page);
	await page.goto(`/prompts/${CHAT}/versions/new`);

	const role = (n: number) => page.getByLabel(`Role of message ${n}`);
	await expect(role(1)).toHaveValue('system');
	await page.getByRole('button', { name: 'Add message' }).click();
	await expect(role(2)).toHaveValue('user');
	// And after that `user`, an `assistant` — the conversation, not a column
	// of the same role.
	await page.getByRole('button', { name: 'Add message' }).click();
	await expect(role(3)).toHaveValue('assistant');
	await page.getByRole('button', { name: 'Remove message 3' }).click();

	await role(2).selectOption('assistant');
	await page.getByLabel('Content of message 2').fill('Certainly.');
	await page.getByLabel('Commit message').fill('an assistant turn');
	await page.getByRole('button', { name: 'Save' }).click();

	await expect(page).toHaveURL(new RegExp(`/prompts/${CHAT}\\?version=4`));
	await expect(page.getByText('assistant', { exact: true })).toBeVisible();
	await expect(page.getByText('Certainly.')).toBeVisible();
});

test('a custom role is saved as it is and opens in the custom field', async ({ page }) => {
	await signIn(page);
	await page.goto(`/prompts/${CHAT}/versions/new`);

	await page.getByLabel('Role of message 1').selectOption({ label: 'Custom…' });
	// The field opens empty, and the Save gate says so at it until it is typed.
	const custom = page.getByLabel('Message 1 custom role');
	await expect(custom).toHaveValue('');
	await expect(page.getByText('A message needs a role.')).toBeVisible();
	await expect(page.getByRole('button', { name: 'Save' })).toBeDisabled();

	await custom.fill('function');
	// Strayed onto a role from the menu and back: what was typed is still there
	// (found in review of PR #45).
	await page.getByLabel('Role of message 1').selectOption('tool');
	await expect(page.getByLabel('Message 1 custom role')).toHaveCount(0);
	await page.getByLabel('Role of message 1').selectOption({ label: 'Custom…' });
	await expect(page.getByLabel('Message 1 custom role')).toHaveValue('function');

	await page.getByLabel('Commit message').fill('a role the menu does not have');
	await page.getByRole('button', { name: 'Save' }).click();

	await expect(page).toHaveURL(new RegExp(`/prompts/${CHAT}\\?version=\\d+`));
	customRole = Number(new URL(page.url()).searchParams.get('version'));
	await expect(page.getByText('function', { exact: true })).toBeVisible();

	// And the editor of the next version opens it where it was typed, rather
	// than losing it to the menu (edge cases).
	await page.getByRole('button', { name: 'New version' }).click();
	await expect(page.getByLabel('Role of message 1').locator('option:checked')).toHaveText('Custom…');
	await expect(page.getByLabel('Message 1 custom role')).toHaveValue('function');
});

// Nothing asks for a `system` message: the one a fresh draft opens with is a
// prefill, and a body that is a single `user` turn is a prompt like any other.
test('a chat prompt with no system message is written and read back', async ({ page }) => {
	const name = 'user-only';
	await signIn(page);
	await page.goto('/prompts/new');
	await page.getByLabel('Name').fill(name);
	await page.getByLabel('Role of message 1').selectOption('user');
	await page.getByLabel('Content of message 1').fill('Summarize {{input}}.');
	await page.getByRole('button', { name: 'Save' }).click();

	await expect(page).toHaveURL(new RegExp(`/prompts/${name}\\?version=1`));
	await expect(page.getByText('user', { exact: true })).toBeVisible();
	await expect(page.getByText('Summarize {{input}}.')).toBeVisible();

	// This suite ends on an empty listing and this name has no part in that
	// story, so it goes back out the way it came in — through the API.
	const gone = await call('DELETE', `/api/v1/prompts/${name}?confirm=${name}`);
	expect(gone.ok).toBeTruthy();
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

// A refresh after a label move used to run without a signal, so it could land
// after a click on another version and overwrite it — the URL saying v1 while
// the pane and the label control were still v3 (found in review of PR #40).
test('a refresh in flight cannot overwrite the version navigated to', async ({ page }) => {
	await signIn(page);
	await page.goto(`/prompts/${CHAT}?version=3`);
	await expect(page.getByRole('heading', { name: 'v3' })).toBeVisible();

	// Hold the re-read of v3 that the label write triggers.
	await page.route(
		(url) => url.pathname.endsWith(`/prompts/${CHAT}`) && url.searchParams.get('version') === '3',
		async (route) => {
			await new Promise((wake) => setTimeout(wake, 2000));
			await route.continue();
		}
	);

	// A label that points nowhere lands straight away and refreshes the page.
	await page.getByRole('button', { name: /Add label/ }).click();
	await page.getByLabel('Label to add').fill('canary');
	await page.getByRole('button', { name: 'Add', exact: true }).click();

	// Off to another version while that re-read is still out.
	await page.getByRole('table', { name: 'Versions' }).getByRole('link', { name: /v1/ }).click();
	await expect(page).toHaveURL(/version=1/);
	await expect(page.getByRole('heading', { name: 'v1' })).toBeVisible();

	// Long enough for the held response to arrive and be ignored.
	await page.waitForTimeout(2500);
	await expect(page.getByRole('heading', { name: 'v1' })).toBeVisible();
	await expect(page.getByRole('heading', { name: 'v3' })).toHaveCount(0);
});

// The optimistic append (spec 021 #14). Without it the *New prompt* screen
// posts to the same endpoint an append does, and quietly extends a name
// somebody else published — moving their labels with it.
test('a name that is taken refuses the create and offers to open it', async ({ page }) => {
	await signIn(page);
	await page.goto('/prompts/new');
	await page.getByLabel('Name').fill(CHAT);
	await page.getByLabel('Content of message 1').fill('Mine now.');
	await page.getByRole('button', { name: 'Save' }).click();

	await expect(page.getByText(/already has a prompt called/)).toBeVisible();
	await expect(page.getByRole('link', { name: 'Open it' })).toHaveAttribute(
		'href',
		new RegExp(`/prompts/${CHAT}\\?version=`)
	);
	// Nothing was written, and Save stays shut until the name is a different
	// one — which is the way out of this particular refusal.
	await expect(page.getByRole('button', { name: 'Save' })).toBeDisabled();
	await page.getByLabel('Name').fill(`${CHAT}-mine`);
	await expect(page.getByRole('button', { name: 'Save' })).toBeEnabled();
});

test('a version that landed under the editor refuses the append', async ({ page }) => {
	await signIn(page);
	await page.goto(`/prompts/${CHAT}/versions/new`);
	await expect(page.getByLabel('Content of message 1')).not.toHaveValue('');

	// Somebody else publishes while this page is open.
	const landed = await call('POST', `/api/v1/prompts/${CHAT}/versions`, {
		prompt: [{ role: 'system', content: 'Published from somewhere else.' }],
		commit_message: 'somebody else'
	});
	expect(landed.ok).toBeTruthy();

	await page.getByLabel('Commit message').fill('composed against the old head');
	await page.getByRole('button', { name: 'Save' }).click();
	await expect(page.getByText(/landed while this was being written/)).toBeVisible();

	// The only way on is to take the version that landed.
	await page.getByRole('button', { name: /Reload from v/ }).click();
	await expect(page.getByLabel('Content of message 1')).toHaveValue(
		'Published from somewhere else.'
	);
	await expect(page.getByRole('button', { name: 'Save' })).toBeEnabled();
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
	// The version the custom-role scenario wrote, not a number written down
	// here: an editor aimed at a version that is not there renders its failure
	// state, which has a heading and no overflow either.
	expect(customRole, 'the version carrying the custom role').not.toBeNull();
	const widest = `/prompts/${CHAT}/versions/new?from=${customRole}`;

	await signIn(page);
	for (const path of [
		'/prompts',
		`/prompts/${CHAT}`,
		`/prompts/${CHAT}?diff=1..3`,
		'/prompts/new',
		`/prompts/${CHAT}/versions/new?from=3`,
		// The custom role (#16) is the widest the role row gets: the select, the
		// field it reveals and the three buttons beside them.
		widest
	]) {
		await page.goto(path);
		await expect(page.getByRole('heading').first()).toBeVisible();
		// And that row is actually on screen, rather than an editor that failed
		// to load being measured instead.
		if (path === widest) {
			await expect(page.getByLabel('Message 1 custom role')).toHaveValue('function');
		}
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

	// The server's own counts, on screen (spec 005 #8) — asked of the API
	// rather than written down here, because the tests above this one add
	// versions and labels and the card must report what is actually there.
	const listing = (await (await call('GET', `/api/v1/prompts/${CHAT}/versions?limit=500`)).json()) as {
		versions: { version: number }[];
		labels: Record<string, number>;
	};
	const card = page.getByRole('dialog');
	await expect(card).toContainText('versions');
	await expect(card.getByText(String(listing.versions.length), { exact: true })).toBeVisible();
	await expect(card).toContainText('labels');
	await expect(
		card.getByText(String(Object.keys(listing.labels).length), { exact: true })
	).toBeVisible();

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
