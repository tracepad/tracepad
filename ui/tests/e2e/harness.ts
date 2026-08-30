import { readFileSync } from 'node:fs';
import { join } from 'node:path';

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
 * The fixture that carries what the wire already sends (spec 012): a release,
 * a tool call, and a generation with a completion start and a prompt link.
 */
export const WIRE_TRACE = 'ff6677008899001122aabb33cc44dd55';
/** Its generation — prompt `support-answer@7`, first token at +388 ms. */
export const WIRE_GENERATION = 'c1c2c3c4c5c6c7c8';
/** Its tool call, which the `type=tool` filter finds the trace by. */
export const WIRE_TOOL = 'd1d2d3d4d5d6d7d8';
