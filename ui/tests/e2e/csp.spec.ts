import { createHash } from 'node:crypto';
import { readdirSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';
import { createProject, signInAsOwner, state } from './harness';

// The page's Content-Security-Policy (spec 050, Testing), against the real
// binary. Three things are claimed and each has a test of its own:
//
//   - the policy is sent, once, with the hash of the script the document
//     carries and no way for any other inline code to run;
//   - it is enforced — code the policy does not name is refused, which is the
//     only thing that makes "no violations" mean something;
//   - every screen of the interface runs under it without a refusal, in both
//     browsers.
//
// The third is the sweep below; the rest of the suite is the other half of it,
// because `fixtures.ts` fails any test in which the browser refused anything.

const FIXTURES = join(resolve(process.cwd(), '..'), 'testdata', 'otlp');

/** The policy's directives of one response, by name. */
function directives(policy: string): Record<string, string> {
	return Object.fromEntries(
		policy.split(';').map((part) => {
			const [name, ...value] = part.trim().split(/\s+/);
			return [name, value.join(' ')];
		})
	);
}

test.describe('what is sent', () => {
	for (const path of ['/', '/login', '/p/anything/dashboard', '/index.html', '/index.html/']) {
		test(`${path} carries one policy, and it names its inline script by hash`, async ({
			request
		}) => {
			const response = await request.get(path);
			expect(response.status()).toBe(200);
			const policies = response
				.headersArray()
				.filter(({ name }) => name.toLowerCase() === 'content-security-policy');
			expect(policies).toHaveLength(1);
			const policy = directives(policies[0].value);

			// What the document actually runs, hashed here by another hand than
			// the server's: a policy naming a hash of something else would be
			// a blank page, and one naming no hash at all would be a hole.
			const inline = [...(await response.text()).matchAll(/<script>([\s\S]*?)<\/script>/g)].map(
				(match) => match[1]
			);
			expect(inline).toHaveLength(1);
			const hash = `'sha256-${createHash('sha256').update(inline[0]).digest('base64')}'`;
			expect(policy['script-src']).toBe(`'self' ${hash}`);

			expect(policy['default-src']).toBe("'none'");
			expect(policies[0].value).not.toMatch(/'unsafe-eval'|'strict-dynamic'/);
			expect(policy['frame-ancestors']).toBe("'none'");
		});
	}

	test('a file of the bundle and an API answer carry the header every response does', async ({
		request
	}) => {
		for (const path of ['/health', '/api/v1']) {
			const response = await request.get(path);
			expect(response.headers()['content-security-policy']).toBe("frame-ancestors 'none'");
		}
	});
});

test.describe('what is refused', () => {
	test.beforeEach(async ({ page }) => {
		await page.goto('/login');
		await expect(page.getByLabel('Email')).toBeVisible();
	});

	// Each of these is something an injected payload would try. The page does
	// it to itself, so the test shows the browser refusing and the code not
	// having run; `csp.take()` hands the refusals to the test, which is the
	// only way a refusal does not fail it.

	test('an inline script, and an inline handler', async ({ page, csp }) => {
		await page.evaluate(() => {
			const script = document.createElement('script');
			script.textContent = 'window.__ranScript = true';
			document.body.append(script);
			document.body.insertAdjacentHTML('beforeend', '<img src="data:," onerror="window.__ranHandler = true">');
		});
		await expect.poll(() => csp.violations.length).toBeGreaterThanOrEqual(2);
		expect(await page.evaluate(() => [(window as any).__ranScript, (window as any).__ranHandler])).toEqual([
			undefined,
			undefined
		]);
		expect(csp.take().map((v) => v.directive)).toEqual(
			expect.arrayContaining(['script-src-elem', 'script-src-attr'])
		);
	});

	test('a script from somewhere else', async ({ page, csp }) => {
		await page.evaluate(() => {
			const script = document.createElement('script');
			script.src = 'http://127.0.0.1:1/elsewhere.js';
			document.body.append(script);
		});
		await expect.poll(() => csp.violations.length).toBeGreaterThanOrEqual(1);
		expect(csp.take()[0]).toMatchObject({
			directive: 'script-src-elem',
			blocked: 'http://127.0.0.1:1/elsewhere.js'
		});
	});

	test('a string run as code', async ({ page, csp }) => {
		// From a listener rather than from `page.evaluate`: a DevTools
		// evaluation is allowed to evaluate whatever the page's policy says,
		// so code run in it would pass for the wrong reason.
		await page.evaluate(() => {
			addEventListener(
				'click',
				() => {
					try {
						(window as unknown as Record<string, unknown>).__outcome = String(new Function('return 1')());
					} catch (cause) {
						(window as unknown as Record<string, unknown>).__outcome = (cause as Error).name;
					}
				},
				{ once: true }
			);
		});
		await page.mouse.click(5, 5);
		await expect.poll(() => page.evaluate(() => (window as any).__outcome)).toBe('EvalError');
		await expect.poll(() => csp.violations.length).toBeGreaterThanOrEqual(1);
		expect(csp.take().map((v) => v.directive)).toContain('script-src');
	});

	test('a request to another origin', async ({ page, csp }) => {
		const outcome = await page.evaluate(() =>
			fetch('http://127.0.0.1:1/', { mode: 'no-cors' }).then(
				() => 'sent',
				() => 'refused'
			)
		);
		expect(outcome).toBe('refused');
		await expect.poll(() => csp.violations.length).toBeGreaterThanOrEqual(1);
		expect(csp.take()[0]).toMatchObject({ directive: 'connect-src' });
	});

	test('a form posted somewhere else', async ({ page, csp }) => {
		await page.evaluate(() => {
			const form = document.createElement('form');
			form.action = 'http://127.0.0.1:1/steal';
			form.method = 'post';
			document.body.append(form);
			form.requestSubmit();
		});
		await expect.poll(() => csp.violations.length).toBeGreaterThanOrEqual(1);
		expect(csp.take()[0]).toMatchObject({ directive: 'form-action' });
	});
});

// --- every screen ------------------------------------------------------------

/** The suite's own project, minted once per worker, and what is in it. */
let own: Promise<Seeded> | null = null;
const seeded = () => (own ??= seed());

type Seeded = {
	project: string;
	dataset: string;
	item: string;
	runA: string;
	runB: string;
	prompt: string;
	queue: string;
	trace: string;
	session: string;
	user: string;
};

async function seed(): Promise<Seeded> {
	const { baseURL } = state();
	const made = await createProject('csp');
	const call = async (method: string, path: string, body?: unknown, type = 'application/json') => {
		const response = await fetch(`${baseURL}${path}`, {
			method,
			headers: { Authorization: `Bearer ${made.key}`, 'Content-Type': type },
			body: body === undefined ? undefined : body instanceof Buffer ? new Uint8Array(body) : JSON.stringify(body)
		});
		if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${await response.text()}`);
		return response;
	};

	for (const name of readdirSync(FIXTURES).filter((file) => file.endsWith('.pb'))) {
		await call('POST', '/v1/traces', readFileSync(join(FIXTURES, name)), 'application/x-protobuf');
	}
	const dataset = 'csp-golden';
	const item = 'a1b2c3d4e5f60718293a4b5c6d7e8f90';
	const runA = '0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7';
	const runB = '1f6b7c1d2b3f4a6980c1d2e3f4a5b6c8';
	const prompt = 'csp-answer';
	const queue = 'csp-review';
	const trace = '4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f';

	await call('PUT', '/api/v1/score-configs/accuracy', { data_type: 'numeric', direction: 'higher', min: 0, max: 1 });
	await call('POST', `/api/v1/datasets/${dataset}/items`, {
		id: item,
		input: { question: 'can I get a refund?' },
		expected_output: { answer: 'Within 30 days.' }
	});
	await call('POST', `/api/v1/datasets/${dataset}/runs`, { id: runA, name: 'prompt v1' });
	await call('POST', `/api/v1/datasets/${dataset}/runs`, { id: runB, name: 'prompt v2' });
	await call('POST', `/api/v1/prompts/${prompt}/versions`, {
		type: 'text',
		prompt: 'Answer {{question}} tersely.',
		commit_message: 'first cut',
		labels: ['production']
	});
	await call('POST', `/api/v1/prompts/${prompt}/versions`, {
		prompt: 'Answer {{question}} tersely and cite the source.',
		commit_message: 'cite'
	});
	await call('PUT', `/api/v1/queues/${queue}`, { score_configs: ['accuracy'] });
	await call('POST', `/api/v1/queues/${queue}/items`, { trace_id: trace });

	// One trace of a user and a session, two hours ago, in OTLP/JSON: the
	// corpus's own are dated before the screens' default window, and the users
	// screens read a rollup the aggregator fills a pass after an hour closes.
	const user = 'csp-user@e2e';
	const session = 'csp-session';
	const start = BigInt(Date.now() - 2 * 3_600_000) * 1_000_000n;
	const text = (key: string, value: string) => ({ key, value: { stringValue: value } });
	await call('POST', '/v1/traces', {
		resourceSpans: [
			{
				resource: { attributes: [text('service.name', 'csp')] },
				scopeSpans: [
					{
						scope: { name: 'e2e' },
						spans: [
							{
								traceId: 'c5b0a1b2c3d4e5f60718293a4b5c6d7e',
								spanId: 'c5b0a1b2c3d4e5f6',
								name: 'answer',
								startTimeUnixNano: String(start),
								endTimeUnixNano: String(start + 500_000_000n),
								attributes: [text('user.id', user), text('session.id', session)]
							}
						]
					}
				]
			}
		]
	});
	await expect
		.poll(
			async () => {
				const body = (await (await call('GET', '/api/v1/users?limit=50')).json()) as {
					users: { user_id: string }[];
				};
				return body.users.some((row) => row.user_id === user);
			},
			{ timeout: 30_000, message: 'the aggregator never rolled the delivered hour' }
		)
		.toBe(true);
	return { project: made.id, dataset, item, runA, runB, prompt, queue, trace, session, user };
}

/**
 * Every screen of the interface, by the route file that makes it, and where to
 * open it. `routesWithAScreen` below reads the same list off the disk, so a
 * screen added without an entry here fails the next run instead of going
 * unwatched.
 */
const SCREENS: Record<string, (s: Seeded) => string> = {
	'/p': () => '/p',
	'/p/[project]': (s) => `/p/${s.project}`,
	'/p/[project]/dashboard': (s) => `/p/${s.project}/dashboard`,
	'/p/[project]/traces': (s) => `/p/${s.project}/traces`,
	'/p/[project]/traces/[id]': (s) => `/p/${s.project}/traces/${s.trace}`,
	'/p/[project]/sessions': (s) => `/p/${s.project}/sessions`,
	'/p/[project]/sessions/[id]': (s) => `/p/${s.project}/sessions/${s.session}`,
	'/p/[project]/users': (s) => `/p/${s.project}/users`,
	'/p/[project]/users/[id]': (s) => `/p/${s.project}/users/${encodeURIComponent(s.user)}`,
	'/p/[project]/prompts': (s) => `/p/${s.project}/prompts`,
	'/p/[project]/prompts/new': (s) => `/p/${s.project}/prompts/new`,
	'/p/[project]/prompts/[name]': (s) => `/p/${s.project}/prompts/${s.prompt}`,
	'/p/[project]/prompts/[name]/versions/new': (s) => `/p/${s.project}/prompts/${s.prompt}/versions/new`,
	'/p/[project]/datasets': (s) => `/p/${s.project}/datasets`,
	'/p/[project]/datasets/[name]': (s) => `/p/${s.project}/datasets/${s.dataset}`,
	'/p/[project]/datasets/[name]/items/[id]/edit': (s) =>
		`/p/${s.project}/datasets/${s.dataset}/items/${s.item}/edit`,
	'/p/[project]/datasets/items/new': (s) => `/p/${s.project}/datasets/items/new?dataset=${s.dataset}`,
	'/p/[project]/runs': (s) => `/p/${s.project}/runs`,
	'/p/[project]/runs/[id]': (s) => `/p/${s.project}/runs/${s.runA}`,
	'/p/[project]/runs/[a]/compare/[b]': (s) => `/p/${s.project}/runs/${s.runA}/compare/${s.runB}`,
	'/p/[project]/score-configs': (s) => `/p/${s.project}/score-configs`,
	'/p/[project]/quality': (s) => `/p/${s.project}/quality`,
	'/p/[project]/queues': (s) => `/p/${s.project}/queues`,
	'/p/[project]/queues/[name]': (s) => `/p/${s.project}/queues/${s.queue}`,
	'/p/[project]/queues/[name]/annotate': (s) => `/p/${s.project}/queues/${s.queue}/annotate`,
	'/p/[project]/settings/project': (s) => `/p/${s.project}/settings/project`,
	'/p/[project]/settings/server': (s) => `/p/${s.project}/settings/server`,
	'/settings/account': () => '/settings/account'
};

/**
 * The screens that are not behind a session, which the sweep opens as nobody.
 * `/setup` and `/invite` are one-time: with an owner already made, the first
 * answers that it is over and the second that a link is not valid, and both of
 * those are screens too. The working versions of them are in `accounts.spec.ts`,
 * which boots a server with no owner, under the same watch.
 */
const OPEN: Record<string, string> = {
	'/login': '/login',
	'/setup': '/setup',
	'/invite': '/invite?token=not-a-token'
};

/** The route files that make a screen, as the paths they serve. */
function routesWithAScreen(): string[] {
	const root = join(process.cwd(), 'src', 'routes');
	return (readdirSync(root, { recursive: true }) as string[])
		.filter((file) => file.endsWith('+page.svelte'))
		.map((file) => '/' + file.replace(/\/?\+page\.svelte$/, ''))
		.map((path) => (path === '/' ? '/' : path))
		.sort();
}

test('every screen of the interface has an entry in the sweep', () => {
	const listed = [...Object.keys(SCREENS), ...Object.keys(OPEN)].sort();
	// `/` has a route file with no screen of its own: it only redirects.
	expect(routesWithAScreen().filter((path) => path !== '/')).toEqual(listed);
});

/** Loads one address and waits until nothing is still on its way. */
async function visit(page: Page, path: string) {
	await page.goto(path);
	await page.waitForLoadState('networkidle');
	// A screen is up when it has said something; the error screen counts.
	await expect(page.locator('h1, h2, [role="alert"]').first()).toBeVisible();
}

test.describe('signed out', () => {
	for (const [route, path] of Object.entries(OPEN)) {
		test(route, async ({ page }) => {
			await visit(page, path);
		});
	}
});

test.describe('signed in', () => {
	for (const route of Object.keys(SCREENS)) {
		test(route, async ({ page }) => {
			const s = await seeded();
			await signInAsOwner(page, s.project);
			await visit(page, SCREENS[route](s));
		});
	}

	test('an address the interface has no screen for', async ({ page }) => {
		const s = await seeded();
		await signInAsOwner(page, s.project);
		await visit(page, `/p/${s.project}/no-such-screen`);
	});
});
