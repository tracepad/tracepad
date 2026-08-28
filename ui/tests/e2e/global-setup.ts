import { spawn, type ChildProcess } from 'node:child_process';
import { mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { PORT, STATE, type State } from './harness';

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
			// Small enough that the corpus's largest payload meets it, which is
			// what puts a truncation marker on the screen to click.
			TRACEPAD_RESPONSE_BUDGET_BYTES: '4096'
		},
		stdio: ['ignore', 'pipe', 'pipe']
	});

	const stop = () => {
		server.kill('SIGTERM');
		rmSync(dataDir, { recursive: true, force: true });
	};

	try {
		const preAuthed = await firstRunURL(server);
		const key = new URLSearchParams(preAuthed.split('#')[1]).get('key');
		if (!key) throw new Error(`no key in the pre-authed URL: ${preAuthed}`);

		const baseURL = `http://127.0.0.1:${PORT}`;
		await waitForHealth(baseURL);
		await ingest(baseURL, key);

		const carried: State = {
			baseURL,
			// The server names itself `localhost`; the tests drive `127.0.0.1`,
			// and a cross-origin hop would drop the localStorage the key lives in.
			preAuthed: preAuthed.replace(/^http:\/\/[^/]+/, baseURL),
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
 * Reads the server's first-run output for the pre-authed link. Waiting for
 * exactly this line is deliberate: the link is part of the contract (spec 006
 * #8), and a boot that stopped printing it should fail the suite rather than
 * be worked around with a key read out of the database.
 */
function firstRunURL(server: ChildProcess): Promise<string> {
	return new Promise((accept, reject) => {
		let output = '';
		const timer = setTimeout(
			() => reject(new Error(`the server printed no pre-authed URL:\n${output}`)),
			20_000
		);
		const read = (chunk: Buffer) => {
			output += chunk.toString();
			const match = /http:\/\/\S+\/#key=tp-sk-[0-9a-f]+/.exec(output);
			if (!match) return;
			clearTimeout(timer);
			accept(match[0]);
		};
		server.stdout?.on('data', read);
		server.stderr?.on('data', read);
		server.on('exit', (code: number | null) => {
			clearTimeout(timer);
			reject(new Error(`the server exited with ${code}:\n${output}`));
		});
	});
}

async function waitForHealth(baseURL: string) {
	for (let attempt = 0; attempt < 100; attempt++) {
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
