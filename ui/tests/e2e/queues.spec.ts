import { readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { expect, test, type Page } from '@playwright/test';
import { createProject, state, WIRE_TRACE } from './harness';

// The annotation loop end to end (spec 024, Testing — e2e), against the real
// binary: declare a queue, fill it by hand and by filter, work it at the desk,
// see the verdicts land on the trace, reopen one, and delete the queue through
// its dry run with the scores still there.
//
// Its own project (spec 016 #18): this suite writes, completes and deletes,
// which is not a thing to do to the project the pagination and stats suites
// count.
//
// Serial, deliberately: the scenario is one story, and each step is the state
// the next one starts from.

test.describe.configure({ mode: 'serial' });

/** Fixture 001: one trace, a span and the generation under it. */
const TRACE = '4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f';
/** Fixture 008's traces, so the filter has more than one thing to match. */
const OTHER_TRACE = WIRE_TRACE;
/** The three traces the two fixtures land, which is what the filter queues. */
const TRACES = 3;

const QUEUE = 'weekly-review';

let own: Promise<{ key: string }> | null = null;
const project = () => (own ??= createProject('queues'));

async function signIn(page: Page) {
	const { key } = await project();
	await page.goto('/login');
	await page.getByLabel('Project key').fill(key);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).toHaveURL(/\/traces$/);
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

/** Two traces and the two configs the queue will ask for. */
function seed(): Promise<void> {
	seeded ??= (async () => {
		for (const name of ['001-langfuse-sdk-generation', '008-wire-columns']) {
			const fixture = readFileSync(
				join(resolve(process.cwd(), '..'), 'testdata', 'otlp', `${name}.pb`)
			);
			await must('POST', '/v1/traces', fixture, 'application/x-protobuf');
		}
		await must('PUT', '/api/v1/score-configs/accuracy', {
			data_type: 'numeric',
			direction: 'higher',
			min: 0,
			max: 1,
			description: 'Did it answer the question'
		});
		await must('PUT', '/api/v1/score-configs/tone', {
			data_type: 'categorical',
			categories: ['warm', 'curt']
		});
	})();
	return seeded;
}

test.beforeEach(async () => {
	await seed();
});

test('a queue is declared over the two configs, and starts empty', async ({ page }) => {
	await signIn(page);
	await page.goto('/queues');

	// The empty state teaches the CLI (spec 016 #15).
	await expect(page.getByText('No queues yet')).toBeVisible();
	await expect(page.getByText(/tracepad queues put/)).toBeVisible();

	await page.getByRole('button', { name: 'New queue' }).click();
	await page.getByLabel('Name').fill(QUEUE);
	await page.getByLabel(/Description/).fill('Did support answer the question?');
	// The gate wants at least one score: a queue is the scores it asks for.
	await expect(page.getByRole('button', { name: 'Create' })).toBeDisabled();
	await page.getByRole('checkbox', { name: /accuracy/ }).check();
	await page.getByRole('checkbox', { name: /tone/ }).check();
	await page.getByRole('button', { name: 'Create' }).click();

	await expect(page).toHaveURL(new RegExp(`/queues/${QUEUE}$`));
	await expect(page.getByText('This queue is empty')).toBeVisible();
});

test('one trace goes in by hand and the rest by filter', async ({ page }) => {
	await signIn(page);

	// The reader's gesture, on the trace itself.
	await page.goto(`/traces/${TRACE}`);
	await page.getByRole('button', { name: 'Add to queue', exact: true }).click();
	await page.getByLabel('Queue', { exact: true }).selectOption(QUEUE);
	await page.getByRole('button', { name: 'Add', exact: true }).click();
	await expect(page.getByText(`Added to ${QUEUE}.`)).toBeVisible();

	// And again: already there, not a second row.
	await page.getByRole('button', { name: 'Add', exact: true }).click();
	await expect(page.getByText(`Already in ${QUEUE}.`)).toBeVisible();

	// The manager's gesture, on the filtered listing. It names the count
	// before the call.
	await page.goto('/traces');
	await page.getByRole('button', { name: 'Add to queue…', exact: true }).click();
	await page.getByLabel('Queue', { exact: true }).selectOption(QUEUE);
	await expect(page.getByText(/traces match these filters/)).toBeVisible();
	await expect(page.getByText(/at most 1,000 are added/)).toBeVisible();
	await page.getByRole('button', { name: 'Add', exact: true }).click();
	// The trace added by hand is one of the matches, and counts as existing.
	await expect(page.getByText(/1 already there/)).toBeVisible();

	await page.goto(`/queues/${QUEUE}`);
	await expect(page.getByRole('row')).toHaveCount(TRACES + 1); // the head and the items
	await expect(page.getByRole('progressbar')).toHaveAttribute('aria-valuemax', String(TRACES));
});

test('the desk asks who is reviewing, refuses Complete until both scores are set, and moves on', async ({
	page
}) => {
	await signIn(page);
	await page.goto(`/queues/${QUEUE}`);
	await page.getByRole('button', { name: 'Start annotating' }).click();

	// Once, and kept in the browser.
	await page.getByLabel('Name').fill('ada');
	await page.getByRole('button', { name: 'Start' }).click();
	// Exact: "metadata" contains "ada", and the trace beside the form has a
	// button for copying it.
	await expect(page.getByRole('button', { name: 'ada', exact: true })).toBeVisible();

	// The trace on the left, the queue's two scores on the right, each built
	// by the rule its config gives.
	await expect(page.getByRole('heading', { name: 'accuracy', exact: true })).toBeVisible();
	await expect(page.getByText('Did it answer the question')).toBeVisible();
	await expect(page.getByText('Still to set: accuracy, tone')).toBeVisible();
	await expect(page.getByRole('button', { name: /Complete/ })).toBeDisabled();

	await page.getByLabel('accuracy', { exact: true }).fill('0.9');
	await expect(page.getByRole('button', { name: /Complete/ })).toBeDisabled();
	await page.getByLabel('tone', { exact: true }).selectOption('warm');
	await page.getByRole('button', { name: /Complete/ }).click();

	// One down, and the desk is on the next item rather than anywhere else.
	// The position is the anchor: the form for the item just finished is
	// replaced by the next one's, so waiting on the number is waiting for the
	// right form to be the one on screen.
	await expect(page.getByText(`1 of ${TRACES}`)).toBeVisible();
	await expect(page.getByTitle('Its place in the queue')).toHaveText('#2');
	await expect(page.getByText('Still to set: accuracy, tone')).toBeVisible();

	// The second one is skipped with a reason. A skip finishes with an item
	// without producing a verdict, so the completed count does not move.
	await page.getByRole('button', { name: 'Skip…' }).click();
	await page.getByLabel(/Why skip/).fill('not a support conversation');
	await page.getByRole('button', { name: 'Skip it' }).click();
	await expect(page.getByText(`1 of ${TRACES}`)).toBeVisible();
	await expect(page.getByTitle('Its place in the queue')).toHaveText('#3');

	// And the last one, so the queue runs out under the reviewer.
	await page.getByLabel('accuracy', { exact: true }).fill('0.4');
	await page.getByLabel('tone', { exact: true }).selectOption('curt');
	await page.getByRole('button', { name: /Complete/ }).click();

	// Nothing left, and it says so rather than showing an empty form.
	await expect(page.getByText('Nothing left in this queue')).toBeVisible();
	await page.getByRole('button', { name: `Back to ${QUEUE}` }).click();
	await expect(page).toHaveURL(new RegExp(`/queues/${QUEUE}$`));
	await expect(page.getByText('not a support conversation')).toBeVisible();
});

test('the verdicts are on the trace, and say they came from the queue', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces/${TRACE}`);

	const accuracy = page.getByRole('button', { name: /accuracy/ });
	await expect(accuracy).toContainText('0.9');
	// The chip is honest about which surface wrote it (#6).
	await expect(accuracy).toContainText('annotation');
	await expect(page.getByRole('button', { name: /tone/ })).toContainText('warm');
});

test('the desk prefills from a verdict already on the trace', async ({ page }) => {
	// A second queue over the same trace: the scores are shared, so its one
	// item can be completed without writing anything (edge cases).
	await must('PUT', `/api/v1/queues/second-look`, {
		description: 'the same trace, another programme',
		score_configs: ['accuracy']
	});
	await must('POST', '/api/v1/queues/second-look/items', { trace_id: TRACE });

	await signIn(page);
	await page.goto('/queues/second-look/annotate');
	// The name lives in the browser, and each test starts in a fresh one.
	await page.getByLabel('Name').fill('ada');
	await page.getByRole('button', { name: 'Start' }).click();

	await expect(page.getByText('already scored')).toBeVisible();
	await expect(page.getByLabel('accuracy', { exact: true })).toHaveValue('0.9');
	// Enabled at once: the shape is already filled.
	await expect(page.getByRole('button', { name: /Complete/ })).toBeEnabled();
	await must('DELETE', '/api/v1/queues/second-look?confirm=second-look');
});

test('a completed item is reopened from the queue page', async ({ page }) => {
	await signIn(page);
	await page.goto(`/queues/${QUEUE}?status=completed`);

	await expect(page.getByRole('row')).toHaveCount(3); // the head and the two done
	await page.getByRole('button', { name: 'Reopen' }).first().click();

	await expect(page.getByRole('row')).toHaveCount(2); // one of them left
	await page.getByLabel('Status').selectOption('pending');
	await expect(page.getByRole('row')).toHaveCount(2); // the head and the reopened one
});

test('deleting the queue asks for its name and leaves the scores', async ({ page }) => {
	await signIn(page);
	await page.goto(`/queues/${QUEUE}`);
	await page.getByRole('button', { name: 'Delete queue' }).click();

	await page.getByRole('button', { name: 'Show what would go' }).click();
	await expect(page.getByText('items', { exact: true })).toBeVisible();
	// The note is the half a reader needs.
	await expect(page.getByText(/only the list goes/)).toBeVisible();

	const execute = page.getByRole('button', { name: 'Delete this queue' });
	await expect(execute).toBeDisabled();
	await page.getByLabel(/Type the queue name/).fill(QUEUE);
	await execute.click();

	await expect(page).toHaveURL(/\/queues$/);
	await expect(page.getByText('No queues yet')).toBeVisible();

	// And the verdicts are where they were: they are the work.
	await page.goto(`/traces/${TRACE}`);
	await expect(page.getByRole('button', { name: /accuracy/ })).toContainText('0.9');
});

// 375 px: the desk stacks the trace over the form, and the page never scrolls
// sideways (Application contract, spec 006 #15).
test('the desk fits a phone', async ({ page }) => {
	await must('PUT', '/api/v1/queues/on-a-phone', { score_configs: ['accuracy'] });
	await must('POST', '/api/v1/queues/on-a-phone/items', { trace_id: OTHER_TRACE });

	await page.setViewportSize({ width: 375, height: 720 });
	await signIn(page);
	await page.goto('/queues/on-a-phone/annotate');
	await page.getByLabel('Name').fill('ada');
	await page.getByRole('button', { name: 'Start' }).click();

	await expect(page.getByLabel('accuracy', { exact: true })).toBeVisible();
	const overflow = await page.evaluate(
		() => document.documentElement.scrollWidth - document.documentElement.clientWidth
	);
	expect(overflow).toBe(0);
	await must('DELETE', '/api/v1/queues/on-a-phone?confirm=on-a-phone');
});
