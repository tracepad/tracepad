import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';
import { createProject, signIn, state } from './harness';

// A user-data erasure is a task on the server (spec 047, Testing — e2e): the
// dialog is answered within its wait when the user is small, follows the
// erasure when it is not, and a reload of the user's page still shows it
// running.
//
// An erasure of a few hundred traces ends in well under the dialog's twenty
// seconds, so the tests that are about an erasure outlasting the wait make it
// outlast it on the wire: the dialog's `wait` is set to zero, which the
// server answers `202` at once, and the reload's reads are held at "running"
// until the test lets the real answers through. Everything else is the real
// server's.

type Project = Awaited<ReturnType<typeof createProject>>;

/** Traces of one user spread over the last hours, in one OTLP/JSON export. */
async function traffic(own: Project, user: string, traces: number) {
	const { baseURL } = state();
	const now = Date.now() * 1_000_000;
	const salt = Math.random().toString(16).slice(2, 10).padEnd(8, '0');
	const spans = Array.from({ length: traces }, (_, n) => {
		const start = now - (n % 6) * 3_600_000_000_000 - n * 1_000_000;
		return {
			traceId: `${salt}${(n + 1).toString(16).padStart(24, '0')}`,
			spanId: `${salt}${(n + 1).toString(16).padStart(8, '0')}`,
			name: 'turn',
			startTimeUnixNano: String(start),
			endTimeUnixNano: String(start + 5_000_000),
			attributes: [{ key: 'langfuse.user.id', value: { stringValue: user } }]
		};
	});
	const response = await fetch(`${baseURL}/v1/traces`, {
		method: 'POST',
		headers: {
			Authorization: `Bearer ${own.key}`,
			'Content-Type': 'application/json'
		},
		body: JSON.stringify({
			resourceSpans: [{ resource: { attributes: [] }, scopeSpans: [{ spans }] }]
		})
	});
	if (!response.ok) throw new Error(`POST /v1/traces: ${response.status} ${await response.text()}`);
}

/** The dialog's confirmed request, answered without waiting for the end. */
async function answerAtOnce(page: Page) {
	await page.route(/\/users\/[^/]+\/data\?.*confirm=/, async (route) => {
		const asked = new URL(route.request().url());
		asked.searchParams.set('wait', '0');
		await route.continue({ url: asked.toString() });
	});
}

test('an erasure that outlasts the dialog is followed to its end, and listed', async ({ page }) => {
	const own = await createProject('erasure-task');
	await traffic(own, 'erase-e2e', 300);
	await signIn(page, own.account);
	await answerAtOnce(page);
	await page.goto('/settings/project');

	await page.getByLabel('User id').fill('erase-e2e');
	await page.getByRole('button', { name: 'Show what would go' }).click();
	await page.getByRole('textbox', { name: /Type the user id/ }).fill('erase-e2e');
	await page.getByRole('button', { name: "Erase this user's data" }).click();

	// Accepted, not answered: the dialog says it runs on, and follows it.
	await expect(page.getByText(/runs on the server; this dialog follows it/)).toBeVisible();
	const progress = page.getByTestId('erasure-progress');
	await expect(progress).toHaveText(/Erasure in progress|Erased 300 traces/);
	await expect(progress).toHaveText(
		'Erased 300 traces belonging to erase-e2e, and 300 spans from 1 raw batch.',
		{
			timeout: 20_000
		}
	);

	// The listing has it, done, and names no one now that it is over.
	const listed = page.getByRole('list', { name: 'Recent erasures' });
	await expect(listed.getByRole('listitem')).toHaveCount(1);
	await expect(listed).toContainText('done');
	await expect(listed).toContainText('300 traces');
	await expect(listed).not.toContainText('erase-e2e');
});

test("a user's page shows an erasure under way, across a reload, until it ends", async ({
	page
}) => {
	const own = await createProject('erasure-reload');
	await traffic(own, 'reload-e2e', 40);
	const { baseURL } = state();
	// The user as the page reads it before the erasure takes it.
	const before = await (
		await fetch(`${baseURL}/api/v1/users/reload-e2e`, {
			headers: { Authorization: `Bearer ${own.key}` }
		})
	).json();
	const started = await fetch(
		`${baseURL}/api/v1/projects/${own.id}/users/reload-e2e/data?confirm=reload-e2e&wait=0`,
		{ method: 'DELETE', headers: { Authorization: `Bearer ${own.key}` } }
	);
	expect(started.status).toBe(202);
	const { id } = (await started.json()) as { id: string };

	// The page's reads say "running" until the test lets them through: the
	// listing it finds the erasure in, the erasure it follows, and the user,
	// still there.
	let held = true;
	await page.route(/\/api\/v1\/users\/reload-e2e(\?|$)/, async (route) => {
		if (held) await route.fulfill({ json: before });
		else await route.continue();
	});
	await page.route(new RegExp(`/projects/${own.id}/erasures$`), async (route) => {
		const answer = await route.fetch();
		const body = await answer.json();
		if (held) {
			for (const one of body.erasures) {
				if (one.id === id)
					Object.assign(one, { state: 'running', phase: 'parsed', user_id: 'reload-e2e' });
			}
		}
		await route.fulfill({ response: answer, json: body });
	});
	await page.route(new RegExp(`/erasures/${id}$`), async (route) => {
		const answer = await route.fetch();
		const body = await answer.json();
		if (held) {
			Object.assign(body, {
				state: 'running',
				phase: 'parsed',
				user_id: 'reload-e2e',
				finished_at: null
			});
			body.progress = { traces_at_start: 40, traces_deleted: 16 };
		}
		await route.fulfill({ response: answer, json: body });
	});

	await signIn(page, own.account);
	await page.goto('/users/reload-e2e');
	const banner = page.getByTestId('erasure-progress');
	await expect(banner).toHaveText('Erasure in progress — parsed, 16 of 40 traces');
	await page.reload();
	await expect(banner).toHaveText('Erasure in progress — parsed, 16 of 40 traces');

	// Let the real server answer: the next read finds it ended.
	held = false;
	await expect(banner).toHaveText(/Erased 40 traces belonging to reload-e2e/, {
		timeout: 10_000
	});
	// The data on screen went with it: the page reads the user again (#30).
	await expect(page.getByRole('heading', { name: /Nothing is filed under/ })).toBeVisible();
});

test("a user's page whose erasure answer was lost shows the data gone once it ended", async ({
	page
}) => {
	const own = await createProject('erasure-lost');
	await traffic(own, 'lost-e2e', 12);
	await signIn(page, own.account);
	// The server erases and answers; a proxy in front of it gives up first.
	await page.route(/\/users\/[^/]+\/data\?.*confirm=/, async (route) => {
		const answer = await route.fetch();
		expect(answer.status()).toBe(200);
		await route.fulfill({ status: 504, body: 'Gateway Timeout' });
	});
	await page.goto('/users/lost-e2e');

	await page.getByRole('button', { name: 'Erase data' }).click();
	await page.getByRole('button', { name: 'Show what would go' }).click();
	await page.getByRole('textbox', { name: /Type the user id/ }).fill('lost-e2e');
	await page.getByRole('button', { name: 'Erase this user’s data' }).click();

	await expect(page.getByText(/The server may have accepted the erasure/)).toBeVisible();
	// It ended, so no listing names the user any more (spec 047 #9): the page
	// reads the user again instead, and finds nothing filed under it (#29).
	await expect(page.getByRole('heading', { name: /Nothing is filed under/ })).toBeVisible();
});
