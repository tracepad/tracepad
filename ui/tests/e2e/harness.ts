import { readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

/** The port the suite boots the binary on. */
export const PORT = Number(process.env.TRACEPAD_E2E_PORT ?? 47318);

/** Whether this run is the merge gate rather than someone's laptop. */
export const CI = !!process.env.CI;

/** Where the boot writes what the tests need to know about it. */
export const STATE = join(process.cwd(), 'tests', 'e2e', '.state.json');

export type State = {
	baseURL: string;
	/** The `#key=` link the server printed on first run (spec 006 #8). */
	preAuthed: string;
	key: string;
};

/**
 * The admin token the suite boots the server with. It is a fixture, not a
 * secret: this server lives for the length of one test run on a temporary
 * database.
 */
export const ADMIN_TOKEN = 'e2e-admin-token';

/**
 * Mints a project of its own, so a test that changes retention, revokes a key
 * or deletes something is not doing it to the project another test is reading.
 * The two Playwright projects run the same files against one server.
 */
export async function createProject(name: string): Promise<{ id: string; name: string; key: string }> {
	const { baseURL } = state();
	const unique = `${name}-${Math.random().toString(36).slice(2, 8)}`;
	const response = await fetch(`${baseURL}/api/v1/projects`, {
		method: 'POST',
		headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, 'Content-Type': 'application/json' },
		body: JSON.stringify({ name: unique })
	});
	if (!response.ok) throw new Error(`create project: ${response.status}`);
	const created = (await response.json()) as { id: string; name: string; secret_key: string };
	return { id: created.id, name: created.name, key: created.secret_key };
}

export function state(): State {
	return JSON.parse(readFileSync(STATE, 'utf8')) as State;
}

/** The fixture whose payloads are large enough to meet a response budget. */
export const LARGE_PAYLOAD_TRACE = '0071122334455667788990aabbccddee';
/** Its generation — the observation that carries the truncated input. */
export const LARGE_PAYLOAD_OBSERVATION = '1112131415161719';
/** The fixture with two failing observations under a healthy root. */
export const FAILING_TRACE = 'dd44ee55ff6677008899001122aabb33';

/**
 * The fixture whose payloads are not JSON: a bare multi-line prompt and its
 * answer, the shape spec 015 #12 shows as text rather than as a document.
 */
export const PLAIN_TEXT_TRACE = 'bc0de1f2a3b4c5d6e7f80910a1b2c3d4';
/** Its only observation, the generation carrying both. */
export const PLAIN_TEXT_OBSERVATION = 'b0b1b2b3b4b5b6b7';

/**
 * What that observation's payloads say, read from the golden the mapper writes
 * rather than copied here. They are a Go string literal (`internal/otlptest`,
 * `plainTextPrompt`), and a copy of one would go stale silently: rewording the
 * prompt regenerates the body and its golden through `make fixtures` and says
 * nothing about a constant in this file, so only the separate e2e job would
 * notice, pointing at a line nobody touched.
 */
export function plainTextPayloads(): { input: string; output: string } {
	const golden = join(
		resolve(process.cwd(), '..'),
		'testdata',
		'golden',
		'011-plain-text-prompt.json'
	);
	const mapped = JSON.parse(readFileSync(golden, 'utf8')) as {
		observations: { input: string; output: string }[];
	};
	return mapped.observations[0];
}

/**
 * The fixture that carries what the wire already sends (spec 012): a release,
 * a tool call, and a generation with a completion start and a prompt link.
 */
export const WIRE_TRACE = 'ff6677008899001122aabb33cc44dd55';
/** Its generation — prompt `support-answer@7`, first token at +388 ms. */
export const WIRE_GENERATION = 'c1c2c3c4c5c6c7c8';
/** Its tool call, which the `type=tool` filter finds the trace by. */
export const WIRE_TOOL = 'd1d2d3d4d5d6d7d8';
/** Its guardrail — prompt `team@acme/answer`, an `@` inside a name and no version. */
export const WIRE_GUARDRAIL = 'e1e2e3e4e5e6e7e8';
