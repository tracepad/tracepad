import { readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { expect, test, type Page } from '@playwright/test';
import { createProject, signIn as enter, state, WIRE_TRACE } from './harness';

// Scores where their target is (spec 022, Testing — e2e), against the real
// binary. The corpus is seeded through the API in a project of its own
// (spec 016 #18): this suite writes, edits and deletes scores, which is not a
// thing to do to the project the pagination and stats suites count.
//
// Serial, and deliberately so: the scenario is one story — read what is there,
// score by hand, correct it, retract it — and each step is the state the next
// one starts from. Both Playwright projects run this file, each in its own
// worker and so in its own project.

test.describe.configure({ mode: 'serial' });

/** Fixture 001: one trace, a span and the generation under it. */
const TRACE = '4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f';
/** Fixture 008's trace: the second one, for the walk between two of them. */
const OTHER_TRACE = WIRE_TRACE;
const GENERATION = '2b3c4d5e6f7a8b9c';
const SESSION = 'session-77';

/** The three scores this suite seeds, by id, so a re-run overwrites them. */
const ON_TRACE = 'aa000000000000000000000000000001';
const ON_OBSERVATION = 'aa000000000000000000000000000002';
const ON_SESSION = 'aa000000000000000000000000000003';
/** A score naming an observation the trace does not carry (edge cases). */
const ON_NOWHERE = 'aa000000000000000000000000000004';

/** The suite's own project, minted once per worker. */
let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('scores'));

async function signIn(page: Page) {
	await enter(page, (await project()).account);
}

async function call(method: string, path: string, body?: unknown, type = 'application/json') {
	const { baseURL } = state();
	const { key } = await project();
	return fetch(`${baseURL}${path}`, {
		method,
		headers: { Authorization: `Bearer ${key}`, 'Content-Type': type },
		body:
			body === undefined
				? undefined
				: body instanceof Uint8Array
					? Buffer.from(body)
					: JSON.stringify(body)
	});
}

async function must(method: string, path: string, body?: unknown, type?: string) {
	const response = await call(method, path, body, type);
	if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${await response.text()}`);
	return response;
}

let seeded: Promise<void> | null = null;

/**
 * Fixture 001's trace, a categorical config for the dialog to build a control
 * from, and one score on each of the three surfaces — plus the one naming an
 * observation that is not in the trace.
 */
function seed(): Promise<void> {
	seeded ??= (async () => {
		for (const name of ['001-langfuse-sdk-generation', '008-wire-columns']) {
			const fixture = readFileSync(
				join(resolve(process.cwd(), '..'), 'testdata', 'otlp', `${name}.pb`)
			);
			await must('POST', '/v1/traces', fixture, 'application/x-protobuf');
		}
		await must('PUT', '/api/v1/score-configs/verdict', {
			data_type: 'categorical',
			categories: ['correct', 'partial', 'wrong'],
			description: 'What the reviewer decided'
		});
		await must('POST', '/api/v1/scores', [
			{ id: ON_TRACE, trace_id: TRACE, name: 'helpfulness', value: 0.875 },
			{
				id: ON_OBSERVATION,
				trace_id: TRACE,
				observation_id: GENERATION,
				name: 'grounded',
				data_type: 'boolean',
				value: 1
			},
			{ id: ON_SESSION, session_id: SESSION, name: 'csat', value: 5 },
			{ id: ON_NOWHERE, trace_id: TRACE, observation_id: 'ffffffffffffffff', name: 'stray', value: 0 }
		]);
	})();
	return seeded;
}

test.beforeEach(async () => {
	await seed();
});

test('the trace header shows its own scores and counts the ones on observations', async ({
	page
}) => {
	await signIn(page);
	await page.goto(`/traces/${TRACE}`);

	// Three significant digits, the source, and the count of what is elsewhere.
	const chip = page.getByRole('button', { name: /helpfulness/ });
	await expect(chip).toContainText('0.875');
	await expect(chip).toContainText('api');
	// One, not two: the stray below names an observation as well, but it is on
	// this header rather than behind a panel, so counting it would send the
	// reader hunting for a score they are looking at (Decision 11).
	await expect(page.getByText('1 more on observation', { exact: true })).toBeVisible();

	// The stray one is in the header, because no panel will ever show it.
	await expect(page.getByRole('button', { name: /stray/ })).toContainText('unknown observation');
	// And the observation's own is not.
	await expect(page.getByRole('button', { name: /grounded/ })).toHaveCount(0);
});

test('the tree badges the observation that was graded, and its panel shows the score', async ({
	page
}) => {
	await signIn(page);
	// Without `?obs=`: a phone shows one pane at a time, and the badge is on
	// the tree — which is the pane a link to a trace opens on.
	await page.goto(`/traces/${TRACE}`);

	await expect(page.getByTitle('1 score on this observation')).toBeVisible();
	await page.getByRole('treeitem', { name: /chat-completion/ }).click();
	const grounded = page.getByRole('button', { name: /grounded/ });
	// Boolean as a word, not as a 1.
	await expect(grounded).toContainText('yes');
	await expect(page.getByRole('button', { name: 'Score this observation' })).toBeVisible();
});

test('the session header shows the session score', async ({ page }) => {
	await signIn(page);
	await page.goto(`/sessions/${SESSION}`);

	await expect(page.getByRole('button', { name: /csat/ })).toContainText('5');
});

test('a score written by hand appears, is corrected in place, and is retracted', async ({
	page
}) => {
	await signIn(page);
	await page.goto(`/traces/${TRACE}`);

	// The configured categorical name builds a select over its categories.
	await page.getByRole('button', { name: 'Score', exact: true }).click();
	await page.getByLabel('Name').selectOption('verdict');
	await page.getByLabel('Value').selectOption('partial');
	await page.getByLabel(/Comment/).fill('half the answer');
	await page.getByRole('button', { name: 'Save' }).click();

	const chip = page.getByRole('button', { name: /verdict/ });
	await expect(chip).toContainText('partial');
	// Written here, so it says so.
	await expect(chip).toContainText('web');

	// Edit: the same dialog, and the same row afterwards.
	const before = await ids('verdict');
	expect(before).toHaveLength(1);
	await chip.click();
	await page.getByRole('button', { name: 'Edit' }).click();
	await page.getByLabel('Value').selectOption('wrong');
	await page.getByRole('button', { name: 'Save' }).click();
	await expect(page.getByRole('button', { name: /verdict/ })).toContainText('wrong');
	expect(await ids('verdict')).toEqual(before);

	// Delete: the dialog names the value, because deleting the wrong verdict
	// is a different mistake from deleting the right one (#5). The chip is
	// still expanded — a correction does not close what was open.
	await page.getByRole('button', { name: 'Delete', exact: true }).click();
	const dialog = page.getByRole('alertdialog');
	await expect(dialog).toContainText('verdict = wrong');
	await dialog.getByRole('button', { name: 'Delete the score' }).click();

	await expect(page.getByRole('button', { name: /verdict/ })).toHaveCount(0);
	expect(await ids('verdict')).toHaveLength(0);
});

test('the free-name path scores a name the project never declared', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces/${TRACE}`);

	await page.getByRole('button', { name: 'Score', exact: true }).click();
	await page.getByLabel('Name').selectOption('other…');
	await page.getByLabel('Score name').fill('vibes');
	// Save waits for the type: nothing else says what the name means (#4).
	await expect(page.getByRole('button', { name: 'Save' })).toBeDisabled();
	await page.getByRole('radio', { name: 'numeric' }).check();
	await page.getByLabel('Value').fill('0.25');
	await page.getByRole('button', { name: 'Save' }).click();

	await expect(page.getByRole('button', { name: /vibes/ })).toContainText('0.25');
	await must('DELETE', `/api/v1/scores/${(await ids('vibes'))[0]}`);
});

// The block reads the scores first and the configs after, so a reader who
// presses *Score* and types straight away is typing before the configs land.
// Seeding the form again when they do empties it under their hands and jumps
// the name off *other…* (found in review of PR #41).
test('the configs landing under an open dialog do not empty it', async ({ page }) => {
	await signIn(page);

	// Hold the config read until the form has something in it.
	let release = () => {};
	const held = new Promise<void>((wake) => (release = wake));
	await page.route('**/api/v1/score-configs*', async (route) => {
		await held;
		await route.continue();
	});

	await page.goto(`/traces/${TRACE}`);
	await page.getByRole('button', { name: 'Score', exact: true }).click();
	await page.getByLabel('Score name').fill('vibes');
	await page.getByRole('radio', { name: 'text' }).check();
	await page.getByLabel('Value').fill('worth keeping');

	release();
	// Long enough for the answer to arrive and be applied.
	await expect(page.getByRole('option', { name: /verdict/ })).toBeAttached();

	await expect(page.getByLabel('Score name')).toHaveValue('vibes');
	await expect(page.getByLabel('Value')).toHaveValue('worth keeping');
	// Still on *other…*, which is where the reader put it.
	await expect(page.getByLabel('Name', { exact: true })).toHaveValue('');
});

// One `Scores` reader serves every trace the panel walks to, and a chip
// carries live Edit and Delete — so trace B must never be drawn wearing trace
// A's judgements (found in review of PR #41).
test('a trace opened over another does not wear its scores', async ({ page }) => {
	await signIn(page);

	// The second trace of the seed, with a score of its own under the same
	// name, so the two answers can be told apart.
	await must('POST', '/api/v1/scores', {
		id: 'aa000000000000000000000000000005',
		trace_id: OTHER_TRACE,
		name: 'helpfulness',
		value: 0.125
	});

	// Hold the second trace's score read, so the swap is caught mid-flight.
	let release = () => {};
	const held = new Promise<void>((wake) => (release = wake));
	await page.route(
		(url) => url.pathname === '/api/v1/scores' && url.searchParams.get('trace_id') === OTHER_TRACE,
		async (route) => {
			await held;
			await route.continue();
		}
	);

	// The panel's own walk, which is what swaps `traceID` without unmounting.
	// A fresh page load would remount the reader and start it empty, which is
	// the one arrangement that cannot go wrong.
	await page.goto(`/traces?peek=${TRACE}`);
	await expect(page.getByRole('button', { name: /helpfulness/ })).toContainText('0.875');

	await page.keyboard.press('k');
	// The row above this one, asserted rather than assumed: on another one the
	// checks below would pass over an empty block and prove nothing.
	await expect(page.getByRole('heading', { name: 'checkout' }).first()).toBeVisible();

	// Nothing of the first trace is on screen while the second is in flight:
	// its chips carry Edit and Delete, and they would act on its scores here.
	await expect(page.getByRole('button', { name: /helpfulness/ })).toHaveCount(0);
	await expect(page.getByRole('button', { name: /stray/ })).toHaveCount(0);

	release();
	await expect(page.getByRole('button', { name: /helpfulness/ })).toContainText('0.125');
});

// The observation panel is a slice of the trace's one read, so it has to
// report that read: without it the panel asserts *No scores* while the request
// is out and goes on asserting it after the request failed, with the header
// right above showing the error (found in review of PR #41).
test('a failed score read is reported on the observation panel too', async ({ page }) => {
	await signIn(page);
	await page.route(`**/api/v1/scores?trace_id=${TRACE}*`, (route) =>
		route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"nope"}' })
	);

	await page.goto(`/traces/${TRACE}`);
	await page.getByRole('treeitem', { name: /chat-completion/ }).click();

	await expect(page.getByText('nope')).toHaveCount(2);
	await expect(page.getByText('No scores')).toHaveCount(0);
});

// A number field answers `''` for what it cannot parse *yet* — the `e` of
// `1e-3`, a lone `-` — and the value is pushed back onto the element from the
// form, so the question is whether that write clears what is being typed. It
// does not: the element already reads `''` at that moment, and Svelte does not
// write a value the element already has. Held here because the whole of that
// argument is somebody else's behaviour (raised in review of PR #41).
test('a number that is typed through an unparseable state survives', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces/${TRACE}`);
	await page.getByRole('button', { name: 'Score', exact: true }).click();
	await page.getByLabel('Name', { exact: true }).selectOption('other…');
	await page.getByLabel('Score name').fill('drift');
	await page.getByRole('radio', { name: 'numeric' }).check();

	const value = page.getByLabel('Value');
	await value.pressSequentially('1e-3');
	await expect(value).toHaveValue('1e-3');

	// A leading `-`, typed over a value that was already there: the state goes
	// from `0.001` to `''` while the element holds a `-` nothing can parse.
	await value.selectText();
	await value.pressSequentially('-0.25');
	await expect(value).toHaveValue('-0.25');

	// And what the form posts is what the field shows.
	await page.getByRole('button', { name: 'Save' }).click();
	await expect(page.getByRole('button', { name: /drift/ })).toContainText('-0.25');
	await must('DELETE', `/api/v1/scores/${(await ids('drift'))[0]}`);
});

test('no screen with a score block scrolls the page sideways', async ({ page }) => {
	await signIn(page);
	for (const path of [
		`/traces/${TRACE}`,
		`/traces/${TRACE}?obs=${GENERATION}`,
		`/sessions/${SESSION}`,
		`/traces?peek=${TRACE}`
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

/** The ids the API has under one name on this trace — the listing, not the screen. */
async function ids(name: string): Promise<string[]> {
	const response = await must(
		'GET',
		`/api/v1/scores?trace_id=${TRACE}&name=${encodeURIComponent(name)}`
	);
	const page = (await response.json()) as { scores: { id: string }[] };
	return page.scores.map((score) => score.id);
}
