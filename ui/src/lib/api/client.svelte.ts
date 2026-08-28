import { auth } from '$lib/auth.svelte';
import type { components, paths } from './schema';

// The whole data layer (spec 006 #7): a thin typed client over the read API,
// nothing more. Every type below is derived from `internal/server/openapi.json`
// through the generated `schema.d.ts`, so a change to the API surface reaches
// this file as a type error rather than as a runtime surprise — and the gate
// fails if the generated file drifts from the document.
//
// No query cache and no store library. Svelte's runes are the reactivity
// model; a second one layered on top is where React idioms and stale-data bugs
// come from.

type JSONResponse<T> = T extends {
	responses: { 200: { content: { 'application/json': infer Body } } };
}
	? Body
	: never;

export type TraceRow = components['schemas']['TraceRow'];
export type Trace = components['schemas']['Trace'];
export type Observation = components['schemas']['Observation'];
export type Truncation = components['schemas']['Truncation'];
export type Expansion = components['schemas']['Expansion'];
export type Project = components['schemas']['Project'];

export type TracePage = JSONResponse<paths['/api/v1/traces']['get']>;
export type ObservationIO = JSONResponse<paths['/api/v1/observations/{id}/io']['get']>;
type ProjectList = JSONResponse<paths['/api/v1/projects']['get']>;

/** Exactly the filters `GET /api/v1/traces` accepts, and nothing else. */
export type TraceFilters = {
	from?: string;
	to?: string;
	environment?: string;
	name?: string;
	user_id?: string;
	session_id?: string;
	tag?: string[];
	status?: 'error' | 'ok';
	min_cost?: string;
};

/**
 * What a credential turns out to be (spec 006 #13). The interface reads
 * traces, so a project key is the only credential it can use; the admin token
 * is named separately because "wrong key" would be a misleading thing to tell
 * someone holding a perfectly valid one.
 */
export type Credential = 'project-key' | 'admin-token' | 'rejected';

/** A request the server answered with something other than success. */
export class ApiError extends Error {
	readonly status: number;

	constructor(status: number, message: string) {
		super(message);
		this.name = 'ApiError';
		this.status = status;
	}

	/** True when the server could not be reached at all. */
	get offline() {
		return this.status === 0;
	}
}

/** The header the server stamps on every response (spec 004 #28). */
const VERSION_HEADER = 'X-Tracepad-Version';

type Query = Record<string, string | string[] | undefined | null>;

class Api {
	/**
	 * The build the server reports. It arrives on whatever request happened to
	 * go out first, which is the point of the header: noticing version skew
	 * should not cost a round trip of its own.
	 */
	version = $state.raw<string | null>(null);

	listProjects(signal?: AbortSignal) {
		return this.#json<ProjectList>('/api/v1/projects', {}, signal);
	}

	listTraces(
		filters: TraceFilters,
		page: { cursor?: string; limit?: number } = {},
		signal?: AbortSignal
	) {
		return this.#json<TracePage>(
			'/api/v1/traces',
			{
				...filters,
				cursor: page.cursor,
				limit: page.limit === undefined ? undefined : String(page.limit)
			},
			signal
		);
	}

	/**
	 * One trace as a tree. `expand=io` asks for the payloads, which the server
	 * inlines or replaces with truncation markers as its budget allows
	 * (spec 004 #2) — the viewer consumes those markers and never second-
	 * guesses the budget.
	 */
	getTrace(id: string, signal?: AbortSignal) {
		return this.#json<Trace>(`/api/v1/traces/${encodeURIComponent(id)}`, { expand: 'io' }, signal);
	}

	/** The one budget-exempt endpoint: a whole payload, on request. */
	getObservationIO(observationId: string, traceId: string, signal?: AbortSignal) {
		return this.#json<ObservationIO>(
			`/api/v1/observations/${encodeURIComponent(observationId)}/io`,
			{ trace_id: traceId },
			signal
		);
	}

	/**
	 * Decides what a credential is, before it is stored (spec 006 #13). The
	 * first request is the one the app actually needs, so a credential that
	 * cannot read the screens cannot get past the login form; the second runs
	 * only to tell the admin token apart from a wrong key, which is the
	 * difference between a useful message and a shrug.
	 */
	async probe(key: string): Promise<Credential> {
		const traces = await this.#fetch('/api/v1/traces', { limit: '1' }, key);
		if (traces.ok) return 'project-key';
		if (traces.status !== 401) throw new ApiError(traces.status, await message(traces));
		const projects = await this.#fetch('/api/v1/projects', {}, key);
		return projects.ok ? 'admin-token' : 'rejected';
	}

	async #json<T>(path: string, query: Query, signal?: AbortSignal): Promise<T> {
		const response = await this.#fetch(path, query, auth.key, signal);
		if (response.ok) return (await response.json()) as T;
		if (response.status === 401) {
			// The stored key is wrong, revoked, or belongs to a project that
			// no longer exists (spec 006 #8). Keeping it would replay the
			// same failure on every screen.
			auth.reject();
			throw new ApiError(401, 'the key was rejected — sign in again');
		}
		throw new ApiError(response.status, await message(response));
	}

	async #fetch(
		path: string,
		query: Query,
		key: string | null,
		signal?: AbortSignal
	): Promise<Response> {
		let response: Response;
		try {
			response = await fetch(path + search(query), {
				headers: key ? { Authorization: `Bearer ${key}` } : {},
				signal
			});
		} catch (cause) {
			if (cause instanceof DOMException && cause.name === 'AbortError') throw cause;
			throw new ApiError(0, 'cannot reach the server');
		}
		const version = response.headers.get(VERSION_HEADER);
		if (version) this.version = version;
		return response;
	}
}

/**
 * Builds a query string the API will accept. Empty values are dropped rather
 * than sent: the read API rejects a parameter given without a value on
 * purpose (spec 004 #23), so an untouched filter field must not appear at all.
 */
export function search(query: Query): string {
	const params = new URLSearchParams();
	for (const [name, value] of Object.entries(query)) {
		if (value === undefined || value === null) continue;
		for (const one of Array.isArray(value) ? value : [value]) {
			if (one !== '') params.append(name, one);
		}
	}
	const encoded = params.toString();
	return encoded ? `?${encoded}` : '';
}

// The API answers every failure with `{"error": "…"}`; anything else is a
// server that is not this one (a proxy, a captive portal), and then the status
// is the only true thing we can say.
async function message(response: Response): Promise<string> {
	try {
		const body = (await response.json()) as { error?: unknown };
		if (typeof body.error === 'string' && body.error) return body.error;
	} catch {
		// Fall through to the status line.
	}
	return `the server answered ${response.status}`;
}

export const api = new Api();
