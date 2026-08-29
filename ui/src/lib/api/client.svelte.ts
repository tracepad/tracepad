import { admin } from '$lib/admin.svelte';
import { auth } from '$lib/auth.svelte';
import type { components, paths } from './schema';
import type { SessionFilters } from './sessions';
import type { TraceFilters } from './traces';

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
export type SessionRow = components['schemas']['SessionRow'];
/** What a destructive request answers before it is confirmed (spec 005 #8). */
export type DryRun = components['schemas']['DryRun'];
/** A freshly minted pair; the secret is in this response and nowhere else. */
export type NewKey = components['schemas']['NewKey'];

export type TracePage = JSONResponse<paths['/api/v1/traces']['get']>;
export type ObservationIO = JSONResponse<paths['/api/v1/observations/{id}/io']['get']>;
export type SessionPage = JSONResponse<paths['/api/v1/sessions']['get']>;
export type Session = JSONResponse<paths['/api/v1/sessions/{id}']['get']>;
export type Stats = JSONResponse<paths['/api/v1/stats']['get']>;
export type KeyList = JSONResponse<paths['/api/v1/projects/{id}/keys']['get']>;
type ProjectList = JSONResponse<paths['/api/v1/projects']['get']>;

/**
 * Which credential a request travels on (spec 007 #3). Two values rather than
 * a boolean, and passed at every call site rather than inferred from the path,
 * because "the admin token never touches the data plane" is a claim that has
 * to be readable in one grep and testable in one spy.
 */
type Scope = 'project' | 'admin';

/** The retention windows a PATCH can move. `null` is "keep forever". */
export type RetentionUpdate = {
	retention_days?: number | null;
	raw_retention_days?: number | null;
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
		return this.#json<ProjectList>('/api/v1/projects', { signal });
	}

	listTraces(
		filters: TraceFilters,
		page: Page = {},
		signal?: AbortSignal
	) {
		return this.#json<TracePage>('/api/v1/traces', {
			query: { ...filters, ...paging(page) },
			signal
		});
	}

	listSessions(
		filters: SessionFilters,
		page: Page = {},
		signal?: AbortSignal
	) {
		return this.#json<SessionPage>('/api/v1/sessions', {
			query: { ...filters, ...paging(page) },
			signal
		});
	}

	/** One session: its totals and a page of its traces. */
	getSession(id: string, page: Page = {}, signal?: AbortSignal) {
		return this.#json<Session>(`/api/v1/sessions/${encodeURIComponent(id)}`, {
			query: paging(page),
			signal
		});
	}

	getStats(
		query: { from?: string; to?: string; environment?: string; group_by?: string },
		signal?: AbortSignal
	) {
		return this.#json<Stats>('/api/v1/stats', { query, signal });
	}

	/**
	 * One trace as a tree. `expand=io` asks for the payloads, which the server
	 * inlines or replaces with truncation markers as its budget allows
	 * (spec 004 #2) — the viewer consumes those markers and never second-
	 * guesses the budget.
	 */
	getTrace(id: string, signal?: AbortSignal) {
		return this.#json<Trace>(`/api/v1/traces/${encodeURIComponent(id)}`, {
			query: { expand: 'io' },
			signal
		});
	}

	/** The one budget-exempt endpoint: a whole payload, on request. */
	getObservationIO(observationId: string, traceId: string, signal?: AbortSignal) {
		return this.#json<ObservationIO>(
			`/api/v1/observations/${encodeURIComponent(observationId)}/io`,
			{ query: { trace_id: traceId }, signal }
		);
	}

	// --- the project's own management (spec 005 #11) -----------------------
	//
	// All of it on the session's project key: a project administers itself.
	// Every destructive one is the server's dry run until `confirm` echoes
	// what it destroys, and the screen renders that preview rather than
	// computing one of its own (spec 007 #5).

	/**
	 * Moves a retention window. Without `confirm` a shrink answers with the
	 * dry run instead of applying — that is the endpoint's contract, and the
	 * discriminator is the `dry_run` field of the body.
	 */
	patchProject(id: string, body: RetentionUpdate, confirm?: string) {
		return this.#json<Project | DryRun>(`/api/v1/projects/${id}`, {
			method: 'PATCH',
			query: { confirm },
			body
		});
	}

	listKeys(id: string, signal?: AbortSignal) {
		return this.#json<KeyList>(`/api/v1/projects/${id}/keys`, { signal });
	}

	createKey(id: string) {
		return this.#json<NewKey>(`/api/v1/projects/${id}/keys`, { method: 'POST' });
	}

	/** Revoking the last key of a project asks for the echo (spec 005 #12). */
	revokeKey(id: string, publicKey: string, confirm?: string) {
		return this.#json<DryRun | { dry_run: false }>(
			`/api/v1/projects/${id}/keys/${encodeURIComponent(publicKey)}`,
			{ method: 'DELETE', query: { confirm } }
		);
	}

	eraseUserData(id: string, userID: string, confirm?: string) {
		return this.#json<DryRun | { dry_run: false; deleted: Record<string, number> }>(
			`/api/v1/projects/${id}/users/${encodeURIComponent(userID)}/data`,
			{ method: 'DELETE', query: { confirm } }
		);
	}

	// --- administration (spec 007 #3, #4) ----------------------------------
	//
	// The five calls below are the only ones in this file that carry the admin
	// token, and they are the project lifecycle plus the rename spec 005 #11
	// made a cross-project act. Nothing that reads trace data appears here.

	/** Every project, soft-deleted ones included with their purge dates. */
	listAllProjects(signal?: AbortSignal) {
		return this.#json<ProjectList>('/api/v1/projects', {
			query: { include: 'deleted' },
			scope: 'admin',
			signal
		});
	}

	createProject(name: string) {
		return this.#json<Project & NewKey>('/api/v1/projects', {
			method: 'POST',
			body: { name },
			scope: 'admin'
		});
	}

	renameProject(id: string, name: string) {
		return this.#json<Project>(`/api/v1/projects/${id}`, {
			method: 'PATCH',
			body: { name },
			scope: 'admin'
		});
	}

	deleteProject(id: string, confirm?: string) {
		return this.#json<DryRun | Project>(`/api/v1/projects/${id}`, {
			method: 'DELETE',
			query: { confirm },
			scope: 'admin'
		});
	}

	restoreProject(id: string) {
		return this.#json<Project>(`/api/v1/projects/${id}/restore`, {
			method: 'POST',
			scope: 'admin'
		});
	}

	/**
	 * Decides whether a token really is the admin token before storing it.
	 * `?include=deleted` is the probe because it is the cheapest request only
	 * the admin token may make: a project key reaches `GET /api/v1/projects`
	 * perfectly well and would otherwise pass for one.
	 */
	async probeAdmin(token: string): Promise<boolean> {
		const response = await this.#fetch('/api/v1/projects', { include: 'deleted' }, token);
		if (response.ok) return true;
		if (response.status === 401 || response.status === 403) return false;
		throw new ApiError(response.status, await message(response));
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

	async #json<T>(path: string, options: Request = {}): Promise<T> {
		const scope = options.scope ?? 'project';
		const key = scope === 'admin' ? admin.token : auth.key;
		if (scope === 'admin' && !key) {
			throw new ApiError(0, 'the Administration section is locked');
		}
		const response = await this.#fetch(path, options.query ?? {}, key, options);
		if (response.ok) return (await response.json()) as T;
		if (response.status === 401) {
			// Whichever credential this request travelled on is wrong,
			// revoked, or belongs to something that no longer exists. Keeping
			// it would replay the same failure on every screen — but a bad
			// admin token must only lock its own section, never sign the
			// reader out of the app (spec 006 #8, spec 007 #3).
			if (scope === 'admin') {
				admin.clear();
				throw new ApiError(401, 'the server did not accept that admin token');
			}
			// The session is over, so both credentials go — the same reason
			// `signOut` clears both. A key that stops working drops whoever
			// was here back to the login form, and the next person to sign in
			// on this browser must not find the management plane already
			// unlocked behind it.
			auth.reject();
			admin.clear();
			throw new ApiError(401, 'the key was rejected — sign in again');
		}
		throw new ApiError(response.status, await message(response));
	}

	async #fetch(
		path: string,
		query: Query,
		key: string | null,
		options: Request = {}
	): Promise<Response> {
		let response: Response;
		try {
			response = await fetch(path + search(query), {
				method: options.method ?? 'GET',
				headers: {
					...(key ? { Authorization: `Bearer ${key}` } : {}),
					...(options.body === undefined ? {} : { 'Content-Type': 'application/json' })
				},
				body: options.body === undefined ? undefined : JSON.stringify(options.body),
				signal: options.signal
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

/** Everything one request can vary. */
type Request = {
	method?: 'GET' | 'POST' | 'PATCH' | 'DELETE';
	query?: Query;
	body?: unknown;
	scope?: Scope;
	signal?: AbortSignal;
};

/** The two page parameters, as query values. */
/**
 * Where a page sits and how big it is (spec 009). `direction` is only sent
 * when it is `prev`, so the common request keeps the shape it had — and
 * `count` only when the caller wants the number, because it is the filters
 * that change it and not the page.
 */
export type Page = {
	cursor?: string;
	limit?: number;
	direction?: 'next' | 'prev';
	count?: boolean;
};

function paging(page: Page): Query {
	return {
		cursor: page.cursor,
		limit: page.limit === undefined ? undefined : String(page.limit),
		direction: page.direction === 'prev' ? 'prev' : undefined,
		count: page.count ? '1' : undefined
	};
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
