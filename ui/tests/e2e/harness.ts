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

export function state(): State {
	return JSON.parse(readFileSync(STATE, 'utf8')) as State;
}

/** The fixture whose payloads are large enough to meet a response budget. */
export const LARGE_PAYLOAD_TRACE = '0071122334455667788990aabbccddee';
/** Its generation — the observation that carries the truncated input. */
export const LARGE_PAYLOAD_OBSERVATION = '1112131415161719';
/** The fixture with two failing observations under a healthy root. */
export const FAILING_TRACE = 'dd44ee55ff6677008899001122aabb33';
