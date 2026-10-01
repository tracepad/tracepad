import { expect, test as base, type BrowserContext } from '@playwright/test';

// Every end-to-end test runs under a watch on the page's Content-Security-
// Policy (spec 051 #7). The interface is served under a policy with no
// `'unsafe-inline'`, so a screen that reaches for an inline script, an inline
// style or an origin the policy does not name does not break loudly: the
// browser refuses it, logs one line to a console nobody reads mid-test, and
// the screen is merely a little wrong. A suite that only looked at what is
// drawn would pass over that. So the tests that already visit every screen
// — in both browsers, at both widths — double as the sweep for it: any
// `securitypolicyviolation` fails the test it happened in.
//
// Every spec takes `test` from here and not from `@playwright/test`, which
// `csp.spec.ts` checks, because a file that forgot would be an unwatched file
// that looks like the others.
//
// And the watch refuses to be vacuous. A policy that is not sent produces no
// violations either, so every HTML document the browser receives must itself
// carry a `script-src`; otherwise "no violations" would be the answer a
// regression gave just as well as the one a clean bundle does.

/** What a browser reports when it refuses something the policy does not allow. */
export type Violation = {
	/** The directive that refused it, e.g. `script-src-elem`. */
	directive: string;
	/** What was refused: a URL, `inline`, `eval`. */
	blocked: string;
	/** The page that asked. */
	page: string;
	/** The script or stylesheet that did, and the line. */
	source: string;
	/** The first characters of the inline code or style, when it was one. */
	sample: string;
};

/** What a watched context has seen so far. */
export type Watch = {
	violations: Violation[];
	/** The HTML documents that arrived with no `script-src` to be held to. */
	unguarded: string[];
	/** Hands back what was seen and forgets it: for a test that provokes one. */
	take(): Violation[];
	/**
	 * Waits until a refusal that is still on its way has arrived. A page
	 * reports a refusal from a task of its own, and the binding carries it to
	 * this process afterwards, so reading `violations` the instant a test ends
	 * can miss the last thing it did. A round trip through every page, with a
	 * timer in it, is behind both.
	 */
	settle(): Promise<void>;
};

const REPORT = '__tracepadCSPViolation';

/**
 * Starts watching every page the context opens, and every frame in them. The
 * listener is an init script so that it is in place before the document's own
 * first script runs, which is the one a policy would refuse first.
 */
export async function watch(context: BrowserContext): Promise<Watch> {
	const seen: Watch = {
		violations: [],
		unguarded: [],
		take() {
			return seen.violations.splice(0);
		},
		async settle() {
			await Promise.all(
				context
					.pages()
					.map((page) => page.evaluate(() => new Promise((done) => setTimeout(done, 50))).catch(() => {}))
			);
		}
	};
	await context.exposeBinding(REPORT, ({ page }, violation: Omit<Violation, 'page'>) => {
		seen.violations.push({ ...violation, page: page.url() });
	});
	await context.addInitScript((name) => {
		addEventListener(
			'securitypolicyviolation',
			(event) => {
				(window as unknown as Record<string, (v: unknown) => void>)[name]({
					directive: event.effectiveDirective,
					blocked: event.blockedURI,
					source: `${event.sourceFile}:${event.lineNumber}`,
					sample: event.sample
				});
			},
			true
		);
	}, REPORT);
	context.on('response', (response) => {
		// A redirect's few words of HTML are never drawn: the browser follows it
		// to the document that is.
		if (response.status() >= 300 && response.status() < 400) return;
		const type = response.headers()['content-type'] ?? '';
		if (!type.startsWith('text/html')) return;
		const policy = response.headers()['content-security-policy'] ?? '';
		if (!/(^|;)\s*script-src\s/.test(policy)) seen.unguarded.push(response.url());
	});
	return seen;
}

/** Fails if the watch saw a refusal, or a document with no policy to refuse with. */
export function expectQuiet(seen: Watch) {
	expect(seen.unguarded, 'HTML documents served without a script-src').toEqual([]);
	expect(seen.violations, 'Content-Security-Policy violations').toEqual([]);
}

/** Lets what is in flight arrive, and then fails as `expectQuiet` does. */
export async function quiet(seen: Watch) {
	await seen.settle();
	expectQuiet(seen);
}

export const test = base.extend<{ csp: Watch }>({
	csp: [
		async ({ context }, use) => {
			const seen = await watch(context);
			await use(seen);
			await quiet(seen);
		},
		{ auto: true }
	],
	// A page is only ever opened after the watch is in place.
	page: async ({ page, csp }, use) => {
		void csp;
		await use(page);
	}
});
