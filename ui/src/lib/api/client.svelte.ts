import { admin } from '$lib/admin.svelte';
import { auth } from '$lib/auth.svelte';
import type { components, paths } from './schema';
import type { RunFilters } from './runs';
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

/** What a write that creates something answers; the items POST is the one here. */
type CreatedResponse<T> = T extends {
	responses: { 201: { content: { 'application/json': infer Body } } };
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

// The eval nouns (spec 014), read by the Evals screens (spec 016).
export type Dataset = components['schemas']['Dataset'];
export type DatasetItem = components['schemas']['DatasetItem'];
export type Run = components['schemas']['Run'];
export type RunWithSummary = components['schemas']['RunWithSummary'];
export type RunScoreStat = components['schemas']['RunScoreStat'];
export type RunItem = components['schemas']['RunItem'];
export type RunAttempt = components['schemas']['RunAttempt'];
export type RunComparison = components['schemas']['RunComparison'];
export type ComparedItem = components['schemas']['ComparedItem'];
export type ComparedScore = components['schemas']['ComparedScore'];
export type ScoreConfig = components['schemas']['ScoreConfig'];

// The scores of spec 003, on screen and written by hand in spec 022.
export type Score = components['schemas']['Score'];
export type ScoreInput = components['schemas']['ScoreInput'];
export type ScorePage = JSONResponse<paths['/api/v1/scores']['get']>;
/** Which target a score listing is about; at most one of the three is set. */
export type ScoreFilters = { trace_id?: string; observation_id?: string; session_id?: string };

// The prompt nouns (spec 003), read and written by the Prompts screens
// (spec 021).
export type Prompt = components['schemas']['Prompt'];
/** What deleting a name takes — the dry run, and what it took. */
export type PromptDeletion = components['schemas']['PromptDeletion'];
export type PromptPage = JSONResponse<paths['/api/v1/prompts']['get']>;
export type PromptRow = PromptPage['prompts'][number];
export type PromptVersionPage = JSONResponse<paths['/api/v1/prompts/{name}/versions']['get']>;
export type PromptVersionRow = PromptVersionPage['versions'][number];
export type PromptDiff = JSONResponse<paths['/api/v1/prompts/{name}/diff']['get']>;
/** What a new version is posted as; the one write these screens make. */
export type PromptVersionInput = NonNullable<
	paths['/api/v1/prompts/{name}/versions']['post']['requestBody']
>['content']['application/json'];

// What the write half sends and gets back (spec 016, PR 2).
export type DatasetItemInput = components['schemas']['DatasetItemInput'];
export type ScoreConfigInput = components['schemas']['ScoreConfigInput'];
/** What deleting a dataset takes — the dry run, and what it took. */
export type DatasetDeletion = components['schemas']['DatasetDeletion'];
export type ItemsWritten = CreatedResponse<paths['/api/v1/datasets/{name}/items']['post']>;
export type ItemArchived = JSONResponse<paths['/api/v1/datasets/{name}/items/{id}']['delete']>;
export type RunDeleted = JSONResponse<paths['/api/v1/runs/{id}']['delete']>;

export type TracePage = JSONResponse<paths['/api/v1/traces']['get']>;
export type DatasetPage = JSONResponse<paths['/api/v1/datasets']['get']>;
export type ItemPage = JSONResponse<paths['/api/v1/datasets/{name}/items']['get']>;
export type ItemVersions = JSONResponse<paths['/api/v1/datasets/{name}/items/{id}/versions']['get']>;
export type RunPage = JSONResponse<paths['/api/v1/runs']['get']>;
export type RunItemPage = JSONResponse<paths['/api/v1/runs/{id}/items']['get']>;
export type ScoreConfigList = JSONResponse<paths['/api/v1/score-configs']['get']>;
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
	stats_retention_days?: number | null;
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
	/**
	 * What the refusal carried beside its sentence. Most carry nothing; a
	 * refusal a client has to *act* on carries what it needs — an append
	 * refused by `expect_version` says which version the name is actually at,
	 * so the screen can offer to open it (spec 021 #14).
	 */
	readonly details: Record<string, unknown>;

	constructor(status: number, message: string, details: Record<string, unknown> = {}) {
		super(message);
		this.name = 'ApiError';
		this.status = status;
		this.details = details;
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

	// --- datasets, runs and score configs (spec 014, read by spec 016) ------
	//
	// One method per endpoint of spec 014's API contract plus the project-wide
	// run listing of spec 016 #2. Every listing pages the same way; the two
	// that take a `version` take it as a query, because "the dataset at V" is
	// the same listing at another instant, not another listing.

	listDatasets(page: Page = {}, signal?: AbortSignal) {
		return this.#json<DatasetPage>('/api/v1/datasets', { query: paging(page), signal });
	}

	getDataset(name: string, signal?: AbortSignal) {
		return this.#json<Dataset>(`/api/v1/datasets/${encodeURIComponent(name)}`, { signal });
	}

	/** The items at a version, whole (spec 014 #19); the current one by default. */
	listItems(name: string, version: number | undefined, page: Page = {}, signal?: AbortSignal) {
		return this.#json<ItemPage>(`/api/v1/datasets/${encodeURIComponent(name)}/items`, {
			query: { version: version === undefined ? undefined : String(version), ...paging(page) },
			signal
		});
	}

	getItem(name: string, id: string, version?: number, signal?: AbortSignal) {
		return this.#json<DatasetItem>(
			`/api/v1/datasets/${encodeURIComponent(name)}/items/${encodeURIComponent(id)}`,
			{ query: { version: version === undefined ? undefined : String(version) }, signal }
		);
	}

	/** Every row of one item's history, newest first, archived rows flagged. */
	listItemVersions(name: string, id: string, signal?: AbortSignal) {
		return this.#json<ItemVersions>(
			`/api/v1/datasets/${encodeURIComponent(name)}/items/${encodeURIComponent(id)}/versions`,
			{ signal }
		);
	}

	listDatasetRuns(name: string, page: Page = {}, signal?: AbortSignal) {
		return this.#json<RunPage>(`/api/v1/datasets/${encodeURIComponent(name)}/runs`, {
			query: paging(page),
			signal
		});
	}

	/** Every dataset's runs, newest first (spec 016 #2). */
	listRuns(filters: RunFilters, page: Page = {}, signal?: AbortSignal) {
		return this.#json<RunPage>('/api/v1/runs', { query: { ...filters, ...paging(page) }, signal });
	}

	getRun(id: string, signal?: AbortSignal) {
		return this.#json<RunWithSummary>(`/api/v1/runs/${encodeURIComponent(id)}`, { signal });
	}

	/** The run's items with their attempts; `unknown` appends the traces no item accounts for. */
	listRunItems(id: string, unknown: boolean, page: Page = {}, signal?: AbortSignal) {
		return this.#json<RunItemPage>(`/api/v1/runs/${encodeURIComponent(id)}/items`, {
			query: { unknown: unknown ? 'true' : undefined, ...paging(page) },
			signal
		});
	}

	compareRuns(a: string, b: string, page: Page = {}, signal?: AbortSignal) {
		return this.#json<RunComparison>(
			`/api/v1/runs/${encodeURIComponent(a)}/compare/${encodeURIComponent(b)}`,
			{ query: paging(page), signal }
		);
	}

	/** Whole, not paged (spec 014 #25). */
	listScoreConfigs(signal?: AbortSignal) {
		return this.#json<ScoreConfigList>('/api/v1/score-configs', { signal });
	}

	// --- writing the eval nouns (spec 016, PR 2) ---------------------------
	//
	// The same endpoints the CLI pushes through, in the same shapes: the
	// screens add no verb of their own. Two of them are declarative `PUT`s —
	// the whole envelope, the whole config — and the item write is a POST
	// whose answer says what it did (spec 014 #6): a version, and how many
	// items produced a row.

	/** The envelope only; the items and the version clock are untouched. */
	putDataset(name: string, body: { description?: string; metadata?: object }) {
		return this.#json<Dataset>(`/api/v1/datasets/${encodeURIComponent(name)}`, {
			method: 'PUT',
			body
		});
	}

	/** A dry run until `confirm` echoes the name (spec 014 #20, spec 005 #8). */
	deleteDataset(name: string, confirm?: string) {
		return this.#json<DatasetDeletion>(`/api/v1/datasets/${encodeURIComponent(name)}`, {
			method: 'DELETE',
			query: { confirm }
		});
	}

	/** One item, added or edited; the id in the body is what makes it an edit. */
	putItem(name: string, item: DatasetItemInput) {
		return this.#json<ItemsWritten>(`/api/v1/datasets/${encodeURIComponent(name)}/items`, {
			method: 'POST',
			body: item
		});
	}

	/** Archives an item at a new version; every earlier version still has it. */
	archiveItem(name: string, id: string) {
		return this.#json<ItemArchived>(
			`/api/v1/datasets/${encodeURIComponent(name)}/items/${encodeURIComponent(id)}`,
			{ method: 'DELETE' }
		);
	}

	/** One row of bookkeeping; the traces it pinned return to the retention window. */
	deleteRun(id: string) {
		return this.#json<RunDeleted>(`/api/v1/runs/${encodeURIComponent(id)}`, { method: 'DELETE' });
	}

	/** Declarative (spec 014 #17): the whole config, so one form creates and edits. */
	putScoreConfig(name: string, body: ScoreConfigInput) {
		return this.#json<ScoreConfig>(`/api/v1/score-configs/${encodeURIComponent(name)}`, {
			method: 'PUT',
			body
		});
	}

	/** The scores the config admitted stay; only the binding goes. */
	deleteScoreConfig(name: string) {
		return this.#json<{ name: string }>(`/api/v1/score-configs/${encodeURIComponent(name)}`, {
			method: 'DELETE'
		});
	}

	// --- scores (spec 003, on screen and written in spec 022) --------------
	//
	// One read and two writes, all three of them endpoints `curl` has had
	// since spec 003 or gained beside these screens (spec 022 #6). The read
	// is filtered by target and asked for once per screen, and the split
	// between the header and the observation panels is rendering (#1).

	listScores(filters: ScoreFilters, page: Page = {}, signal?: AbortSignal) {
		return this.#json<ScorePage>('/api/v1/scores', {
			query: { ...filters, ...paging(page) },
			signal
		});
	}

	/** One score. An `id` in the body makes it a correction rather than a new row. */
	createScore(body: ScoreInput) {
		return this.#json<{ ids: string[] }>('/api/v1/scores', { method: 'POST', body });
	}

	/** A retraction; posting the same id again puts the row back (spec 022 #6). */
	deleteScore(id: string) {
		return this.#json<{ id: string }>(`/api/v1/scores/${encodeURIComponent(id)}`, {
			method: 'DELETE'
		});
	}

	// --- prompts (spec 003, on screen in spec 021) -------------------------
	//
	// One method per endpoint, the write half included: a version is appended
	// (the store is append-only, so there is no edit), a label is pointed or
	// unpointed, and a name is deleted whole behind the echo. The screens add
	// no verb of their own — every one of these is a `curl` in docs/prompts.md.

	listPrompts(page: Page = {}, signal?: AbortSignal) {
		return this.#json<PromptPage>('/api/v1/prompts', { query: paging(page), signal });
	}

	/** One version: the one named, the one a label points at, or the latest. */
	getPrompt(name: string, at: { version?: number; label?: string } = {}, signal?: AbortSignal) {
		return this.#json<Prompt>(`/api/v1/prompts/${encodeURIComponent(name)}`, {
			query: { version: at.version === undefined ? undefined : String(at.version), label: at.label },
			signal
		});
	}

	/** Newest first, without the bodies: a version list is for picking and diffing. */
	listPromptVersions(name: string, page: Page = {}, signal?: AbortSignal) {
		return this.#json<PromptVersionPage>(
			`/api/v1/prompts/${encodeURIComponent(name)}/versions`,
			{ query: paging(page), signal }
		);
	}

	/** The server's unified patch; the interface computes no diff (spec 021 #3). */
	promptDiff(name: string, from: number, to: number, signal?: AbortSignal) {
		return this.#json<PromptDiff>(`/api/v1/prompts/${encodeURIComponent(name)}/diff`, {
			query: { from: String(from), to: String(to) },
			signal
		});
	}

	createPromptVersion(name: string, body: PromptVersionInput) {
		return this.#json<Prompt>(`/api/v1/prompts/${encodeURIComponent(name)}/versions`, {
			method: 'POST',
			body
		});
	}

	/** The deploy path: promote by moving it forward, roll back by moving it back. */
	putPromptLabel(name: string, label: string, version: number) {
		return this.#json<{ label: string; version: number }>(
			`/api/v1/prompts/${encodeURIComponent(name)}/labels/${encodeURIComponent(label)}`,
			{ method: 'PUT', body: { version } }
		);
	}

	deletePromptLabel(name: string, label: string) {
		return this.#json<{ label: string; version: number }>(
			`/api/v1/prompts/${encodeURIComponent(name)}/labels/${encodeURIComponent(label)}`,
			{ method: 'DELETE' }
		);
	}

	/** A dry run until `confirm` echoes the name (spec 021 #7, spec 005 #8). */
	deletePrompt(name: string, confirm?: string) {
		return this.#json<PromptDeletion>(`/api/v1/prompts/${encodeURIComponent(name)}`, {
			method: 'DELETE',
			query: { confirm }
		});
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
		throw await refusal(response);
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
		if (traces.status !== 401) throw await refusal(traces);
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
		throw await refusal(response);
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
				// Never from the browser's cache (spec 021 #13). The prompt
				// reads carry `Cache-Control: max-age=60` for the SDKs that
				// poll them by label (spec 003 #14), and a screen that has
				// just moved a label would otherwise re-read the minute-old
				// answer and show the move as not having happened. Every other
				// endpoint sends no caching headers, so this changes nothing
				// for them and states what the data plane is: live.
				cache: 'no-store',
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
	method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
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
// is the only true thing we can say. Whatever else the envelope carries travels
// with it as `details`, for the refusals a screen has to act on rather than
// only show (spec 021 #14).
async function refusal(response: Response): Promise<ApiError> {
	try {
		const body = (await response.json()) as Record<string, unknown>;
		const { error, ...rest } = body;
		if (typeof error === 'string' && error) return new ApiError(response.status, error, rest);
	} catch {
		// Fall through to the status line.
	}
	return new ApiError(response.status, `the server answered ${response.status}`);
}

export const api = new Api();
