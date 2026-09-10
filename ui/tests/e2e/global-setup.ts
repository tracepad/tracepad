import { spawn, type ChildProcess } from 'node:child_process';
import { mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { ADMIN_TOKEN, PORT, STATE, type State } from './harness';

// Boots the real binary on a temp database and fills it from the synthetic
// OTLP corpus in `testdata/` (spec 006, Testing). Nothing here is mocked: the
// point of this suite is exactly the seam the unit layer cannot see — the
// bundle inside the binary, the SPA fallback, and the URL the server prints.

const ROOT = resolve(process.cwd(), '..');
const BINARY = join(ROOT, 'bin', 'tracepad');
const FIXTURES = join(ROOT, 'testdata', 'otlp');

export default async function boot() {
	const dataDir = mkdtempSync(join(tmpdir(), 'tracepad-e2e-'));
	const server = spawn(BINARY, ['serve', '--listen', `127.0.0.1:${PORT}`], {
		env: {
			...process.env,
			TRACEPAD_DATA_DIR: dataDir,
			// The management-plane credential (spec 005 #11), which the
			// Settings screen's Administration section is entered with.
			TRACEPAD_ADMIN_TOKEN: ADMIN_TOKEN,
			// Small enough that the corpus's largest payload meets it, which is
			// what puts a truncation marker on the screen to click.
			TRACEPAD_RESPONSE_BUDGET_BYTES: '4096',
			// The aggregator at its floor (spec 023, Testing). The Users
			// listing answers from the rollup alone, so without a pass there
			// is nothing on it; the fixtures are stamped in the past, so
			// every hour they fall in is closed from the first pass on.
			TRACEPAD_ROLLUP_INTERVAL: '1s'
		},
		stdio: ['ignore', 'pipe', 'pipe']
	});

	const stop = () => {
		server.kill('SIGTERM');
		rmSync(dataDir, { recursive: true, force: true });
	};

	try {
		const { key, setup } = await firstRunOutput(server);

		const baseURL = `http://127.0.0.1:${PORT}`;
		await waitForHealth(baseURL, server);
		await ingest(baseURL, key);

		const carried: State = {
			baseURL,
			// The server names itself `localhost`; the tests drive `127.0.0.1`,
			// and a cross-origin hop would drop the localStorage the key lives
			// in and the cookie a session lives in.
			setup: setup.replace(/^http:\/\/[^/]+/, baseURL),
			preAuthed: `${baseURL}/#key=${key}`,
			key
		};
		writeFileSync(STATE, JSON.stringify(carried, null, 2));
	} catch (cause) {
		stop();
		throw cause;
	}

	return () => {
		stop();
		rmSync(STATE, { force: true });
	};
}

/**
 * Reads the server's first-run output for the two things it prints: the
 * project's secret key, and the setup link that creates the first owner
 * (spec 028 #9). Waiting for exactly these is deliberate — both are part of
 * the contract, and a boot that stopped printing one should fail the suite
 * rather than be worked around with a value read out of the database.
 */
function firstRunOutput(server: ChildProcess): Promise<{ key: string; setup: string }> {
	return new Promise((accept, reject) => {
		let output = '';
		const timer = setTimeout(
			() => reject(new Error(`the server printed no first-run output:\n${output}`)),
			20_000
		);
		const read = (chunk: Buffer) => {
			output += chunk.toString();
			const key = /authorization=Bearer (tp-sk-[0-9a-f]+)/.exec(output);
			const setup = /http:\/\/\S+\/setup#token=\S+/.exec(output);
			if (!key || !setup) return;
			clearTimeout(timer);
			accept({ key: key[1], setup: setup[0] });
		};
		server.stdout?.on('data', read);
		server.stderr?.on('data', read);
		server.on('exit', (code: number | null) => {
			clearTimeout(timer);
			reject(new Error(`the server exited with ${code}:\n${output}`));
		});
	});
}

async function waitForHealth(baseURL: string, server: ChildProcess) {
	for (let attempt = 0; attempt < 100; attempt++) {
		// A server that has already exited is never going to answer, and
		// something else may well be answering on its port — a leftover from an
		// interrupted run, most often. Without this check the suite went on to
		// ingest into a stranger's database and reported a bare `401`.
		if (server.exitCode !== null) {
			throw new Error(
				`the server exited with ${server.exitCode} before becoming healthy; ` +
					`is something already listening on ${PORT}?`
			);
		}
		try {
			if ((await fetch(`${baseURL}/health`)).ok) return;
		} catch {
			// Not listening yet.
		}
		await new Promise((wake) => setTimeout(wake, 100));
	}
	throw new Error('the server never became healthy');
}

/** The synthetic corpus, exported the way an SDK would export it. */
async function ingest(baseURL: string, key: string) {
	for (const name of readdirSync(FIXTURES).filter((file: string) => file.endsWith('.pb'))) {
		const response = await fetch(`${baseURL}/v1/traces`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/x-protobuf', Authorization: `Bearer ${key}` },
			body: readFileSync(join(FIXTURES, name))
		});
		if (!response.ok) throw new Error(`ingest ${name}: ${response.status}`);
	}
}
