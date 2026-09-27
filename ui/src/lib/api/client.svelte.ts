import { untrack } from 'svelte';
import { auth } from '$lib/auth.svelte';
import { project } from '$lib/project.svelte';
import type { components, paths } from './schema';
import type { QueueItemFilters } from './queues';
import type { RunFilters } from './runs';
import type { SessionFilters } from './sessions';
import type { TraceFilters } from './traces';
import type { UserFilters } from './users';

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
/** One end user's roll-up (spec 023); the same shape the listing and the page read. */
export type UserRow = components['schemas']['UserRow'];
/** What a destructive request answers before it is confirmed (spec 005 #8). */
export type DryRun = components['schemas']['DryRun'];
/** What deleting one trace removed (spec 035 #1). */
export type TraceDeletion = components['schemas']['TraceDeletion'];
/** What one round of a deletion by filter removed, and whether there is more (spec 035 #4). */
export type TracesDeletion = components['schemas']['TracesDeletion'];
/** A freshly minted pair; the secret is in this response and nowhere else. */
export type NewKey = components['schemas']['NewKey'];
/** One of a project's keys, with who minted it and when it was last used (spec 045). */
export type Key = components['schemas']['Key'];

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

// The annotation nouns (spec 024): a review programme and the pointers in it.
export type AnnotationQueue = components['schemas']['AnnotationQueue'];
export type AnnotationItem = components['schemas']['AnnotationItem'];
export type QueueTarget = components['schemas']['QueueTarget'];
/** What deleting a queue takes — the dry run, and what it took. */
export type QueueDeletion = components['schemas']['QueueDeletion'];
export type QueueList = JSONResponse<paths['/api/v1/queues']['get']>;
export type QueueItemPage = JSONResponse<paths['/api/v1/queues/{name}/items']['get']>;
export type NextItem = JSONResponse<paths['/api/v1/queues/{name}/next']['get']>;
export type ItemsAdded = CreatedResponse<paths['/api/v1/queues/{name}/items']['post']>;
export type TracesQueued = CreatedResponse<
	paths['/api/v1/queues/{name}/items/from-traces']['post']
>;

// The scores of spec 003, on screen and written by hand in spec 022.
export type Score = components['schemas']['Score'];
export type ScoreInput = components['schemas']['ScoreInput'];
export type ScorePage = JSONResponse<paths['/api/v1/scores']['get']>;
/** The quality trend (spec 025): one series per score name, each with buckets. */
export type ScoreSeries = components['schemas']['ScoreSeries'];
export type ScoreBucket = components['schemas']['ScoreBucket'];
export type ScoreTrends = JSONResponse<paths['/api/v1/stats/scores']['get']>;

/** The filter values of a range, and what the cap left out (spec 027 #2). */
export type Facets = JSONResponse<paths['/api/v1/facets']['get']>;
export type FacetValue = components['schemas']['FacetValue'];
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
export type UserPage = JSONResponse<paths['/api/v1/users']['get']>;
export type User = JSONResponse<paths['/api/v1/users/{id}']['get']>;
export type Stats = JSONResponse<paths['/api/v1/stats']['get']>;
export type KeyList = JSONResponse<paths['/api/v1/projects/{id}/keys']['get']>;
type ProjectList = JSONResponse<paths['/api/v1/projects']['get']>;

// The people who sign in (spec 028). An `Account` is how somebody sees
// themselves; an `AccountDetail` is how an owner sees them, which is the same
// row plus what an owner manages — standing, invitation, last login, projects.
export type Account = components['schemas']['Account'];
export type AccountDetail = components['schemas']['AccountDetail'];
/** One project an account can reach, with the role it has there. */
export type Membership = components['schemas']['Membership'];
/** Who is signed in and what they can reach: the one call the shell makes. */
export type Me = components['schemas']['Me'];
/** One browser's sign-in, as the session list shows it. */
export type AccountSession = components['schemas']['AccountSession'];
/** A new account and the link that sets its password, shown this once. */
export type Invitation = components['schemas']['Invitation'];
/** What deleting an account would take, and the email that makes it happen. */
export type AccountDeletion = components['schemas']['AccountDeletion'];
/** What an account is created or edited with; an owner has no memberships. */
export type AccountInput = {
	email?: string;
	name?: string;
	owner?: boolean;
	memberships?: { project_id: string; role: MemberRole }[];
};
/** The two roles a membership can carry; `owner` is a flag, not a role here. */
export type MemberRole = 'viewer' | 'editor';
/** The project side of the question: who has a role in this one. */
export type MemberList = JSONResponse<paths['/api/v1/projects/{id}/members']['get']>;

/** The retention windows a PATCH can move. `null` is "keep forever". */
export type RetentionUpdate = {
	retention_days?: number | null;
	raw_retention_days?: number | null;
	stats_retention_days?: number | null;
	/** What ingest does with media it takes out of a payload (spec 041 #6). */
	media?: 'store' | 'placeholder';
};

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

/**
 * How long any one request may go unanswered (spec 010 #10). A request that
 * neither resolves nor rejects is the one failure nothing above this layer can
 * see: `loading` stays up forever, and a live tick that never settles holds
 * the listing's one-read-at-a-time gate with `liveFailure` still `null` — the
 * screen freezes silently and the toggle does not free it. The read API
 * answers in milliseconds and caps every scan, so thirty seconds is not a
 * budget a slow query spends; it is the line past which "still waiting" is
 * "no answer".
 */
export const REQUEST_TIMEOUT_MS = 30_000;

type Query = Record<string, string | string[] | undefined | null>;

class Api {
	/**
	 * The build the server reports. It arrives on whatever request happened to
	 * go out first, which is the point of the header: noticing version skew
	 * should not cost a round trip of its own.
	 */
	version = $state.raw<string | null>(null);

	/**
	 * The projects this session reaches. `activity: '24h'` puts each row's
	 * traces of the last day on it (spec 029 #8), which is what the switcher
	 * asks for on every open and nothing asks for on load.
	 */
	listProjects(query: { activity?: '24h' } = {}, signal?: AbortSignal) {
		return this.#json<ProjectList>('/api/v1/projects', { query, signal });
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

	/**
	 * Deleting one trace (spec 035 #1): a dry run until `confirm` echoes the
	 * trace id, which is its only identity.
	 */
	deleteTrace(id: string, confirm?: string) {
		return this.#json<DryRun | TraceDeletion>(`/api/v1/traces/${encodeURIComponent(id)}`, {
			method: 'DELETE',
			query: { confirm }
		});
	}

	/**
	 * Deleting every trace a filter matches before `to` (spec 035 #2, #4):
	 * a dry run until `confirm` echoes the project's name, then one round of
	 * at most `limit` traces per call, repeated while the answer says `more`.
	 * The filters must carry `to`; the server refuses them without it.
	 */
	deleteTraces(filters: TraceFilters, confirm?: string, limit?: number) {
		return this.#json<DryRun | TracesDeletion>('/api/v1/traces', {
			method: 'DELETE',
			query: { ...filters, confirm, limit: limit === undefined ? undefined : String(limit) }
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

	/**
	 * The user listing (spec 023). It answers from the rollup alone, so a user
	 * first seen minutes ago is not on it yet — which is why the empty state
	 * and `docs/users.md` both say so, and why `getUser` exists for any id.
	 */
	listUsers(filters: UserFilters, page: Page = {}, signal?: AbortSignal) {
		return this.#json<UserPage>('/api/v1/users', {
			query: { ...filters, ...paging(page) },
			signal
		});
	}

	/** One user: the rollup merged with the live tail, exact for any id. */
	getUser(id: string, signal?: AbortSignal) {
		return this.#json<User>(`/api/v1/users/${encodeURIComponent(id)}`, { signal });
	}

	getStats(
		query: {
			from?: string;
			to?: string;
			environment?: string;
			user_id?: string;
			group_by?: string;
		},
		signal?: AbortSignal
	) {
		return this.#json<Stats>('/api/v1/stats', { query, signal });
	}

	/**
	 * What the three many-valued filters can be set to, for the range in view
	 * (spec 027 #2): one request, three lists with counts. It takes only the
	 * range — the counts do not respect the other filters, so the list does
	 * not move as boxes are ticked.
	 */
	getFacets(query: { from?: string; to?: string }, signal?: AbortSignal) {
		return this.#json<Facets>('/api/v1/facets', { query, signal });
	}

	/**
	 * The quality trend (spec 025): a series per score name over the same
	 * window the statistics answer for. Without `name` every name in the range
	 * comes back, which is the one request the overview needs.
	 */
	getScoreTrends(
		query: {
			from?: string;
			to?: string;
			environment?: string;
			name?: string;
			group_by?: string;
		},
		signal?: AbortSignal
	) {
		return this.#json<ScoreTrends>('/api/v1/stats/scores', { query, signal });
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

	/**
	 * One media body by its hash (spec 041 #7), as a blob the page can show
	 * without the bytes ever being rendered as a document of this origin. The
	 * browser may cache it: the address is the content.
	 */
	async media(sha: string, signal?: AbortSignal): Promise<Blob> {
		const response = await this.#fetch(
			`/api/v1/media/${sha}`,
			{},
			{ signal, cache: 'default', clockUntilHeaders: true }
		);
		if (response.ok) return await response.blob();
		throw await refusal(response);
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

	// --- annotation queues (spec 024) --------------------------------------
	//
	// One method per endpoint of the spec's API contract, in the shapes the
	// CLI uses: the screens add no verb of their own. `next` is a GET that
	// claims — being handed an item *is* the claim (#5) — and the three
	// finishing writes carry the annotator, because the store has no users
	// and this spec does not invent them (#6).

	/** Whole, not paged: a project has as many queues as review programmes. */
	listQueues(signal?: AbortSignal) {
		return this.#json<QueueList>('/api/v1/queues', { signal });
	}

	getQueue(name: string, signal?: AbortSignal) {
		return this.#json<AnnotationQueue>(`/api/v1/queues/${encodeURIComponent(name)}`, { signal });
	}

	/** Declarative (#1): the whole queue, so one form creates and replaces. */
	putQueue(name: string, body: { description?: string; score_configs: string[] }) {
		return this.#json<AnnotationQueue>(`/api/v1/queues/${encodeURIComponent(name)}`, {
			method: 'PUT',
			body
		});
	}

	/** A dry run until `confirm` echoes the name; the scores stay (#3, #8). */
	deleteQueue(name: string, confirm?: string) {
		return this.#json<QueueDeletion>(`/api/v1/queues/${encodeURIComponent(name)}`, {
			method: 'DELETE',
			query: { confirm }
		});
	}

	/** One target or many, all or nothing; a repeat counts as `existing`. */
	addQueueItems(name: string, targets: QueueTarget | QueueTarget[]) {
		return this.#json<ItemsAdded>(`/api/v1/queues/${encodeURIComponent(name)}/items`, {
			method: 'POST',
			body: targets
		});
	}

	/** Every trace the listing's filters match, newest first, capped (#4). */
	queueFromTraces(name: string, filters: TraceFilters, limit: number) {
		return this.#json<TracesQueued>(
			`/api/v1/queues/${encodeURIComponent(name)}/items/from-traces`,
			{ method: 'POST', query: { ...filters, limit: String(limit) } }
		);
	}

	listQueueItems(name: string, filters: QueueItemFilters, page: Page = {}, signal?: AbortSignal) {
		return this.#json<QueueItemPage>(`/api/v1/queues/${encodeURIComponent(name)}/items`, {
			query: { ...filters, ...paging(page) },
			signal
		});
	}

	/** The item to work on, claimed for ten minutes; null when there is none. */
	nextQueueItem(name: string, annotator: string, signal?: AbortSignal) {
		return this.#json<NextItem>(`/api/v1/queues/${encodeURIComponent(name)}/next`, {
			query: { annotator },
			signal
		});
	}

	/** Refused with `missing` until the queue's scores are on the target (#7). */
	completeQueueItem(name: string, id: string, annotator: string) {
		return this.#finishItem(name, id, 'complete', { annotator });
	}

	skipQueueItem(name: string, id: string, annotator: string, reason: string) {
		return this.#finishItem(name, id, 'skip', { annotator, reason });
	}

	reopenQueueItem(name: string, id: string, annotator: string) {
		return this.#finishItem(name, id, 'reopen', { annotator });
	}

	/** One row out of the list; a re-add recreates it, so no ceremony (#8). */
	deleteQueueItem(name: string, id: string) {
		return this.#json<{ id: string }>(
			`/api/v1/queues/${encodeURIComponent(name)}/items/${encodeURIComponent(id)}`,
			{ method: 'DELETE' }
		);
	}

	#finishItem(name: string, id: string, verb: string, body: object) {
		return this.#json<AnnotationItem>(
			`/api/v1/queues/${encodeURIComponent(name)}/items/${encodeURIComponent(id)}/${verb}`,
			{ method: 'POST', body }
		);
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
	// All of it on the session, for the project on screen. The keys are an
	// owner's or an editor's: no project key manages keys (spec 045 #4).
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

	/** A name says which program will hold the key (spec 045 #6). */
	createKey(id: string, name = '') {
		return this.#json<NewKey>(`/api/v1/projects/${id}/keys`, {
			method: 'POST',
			body: name ? { name } : undefined
		});
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

	// --- signing in (spec 028 #8, #9, #10) ---------------------------------
	//
	// The four calls that work without a session, and the three that are about
	// the session itself. Nothing here is stored by this file: the cookie is
	// the credential and the browser holds it, which is the whole point of
	// Decision 4.

	/** Whether this server still needs its first owner; askable by anybody. */
	getSetup(signal?: AbortSignal) {
		return this.#json<{ required: boolean; enabled: boolean; expired: boolean }>('/api/v1/setup', {
			anonymous: true,
			signal
		});
	}

	/** Creates the first owner from the token the server printed. */
	setup(body: { token: string; email: string; password: string; name?: string }) {
		return this.#json<{ account: Account }>('/api/v1/setup', {
			method: 'POST',
			body,
			anonymous: true
		});
	}

	login(email: string, password: string) {
		return this.#json<{ account: Account }>('/api/v1/auth/login', {
			method: 'POST',
			body: { email, password },
			anonymous: true
		});
	}

	/** Sets a password from an invitation link and signs in; single-use. */
	acceptInvite(token: string, password: string) {
		return this.#json<{ account: Account }>('/api/v1/auth/accept-invite', {
			method: 'POST',
			body: { token, password },
			anonymous: true
		});
	}

	logout() {
		return this.#json<void>('/api/v1/auth/logout', { method: 'POST', anonymous: true });
	}

	/**
	 * Who is signed in and what they can reach. `anonymous` because a 401 here
	 * is the guard's answer rather than an accident: it is how the shell learns
	 * there is nobody to render for, and bouncing from inside the client would
	 * race the redirect the guard is about to make.
	 */
	me(signal?: AbortSignal) {
		return this.#json<Me>('/api/v1/auth/me', { anonymous: true, signal });
	}

	/**
	 * A display name, a password, the preferences, or any of them together;
	 * a password change needs the old one, and the preferences replace the
	 * stored object whole (spec 034 #9).
	 */
	patchMe(body: {
		name?: string;
		password?: { current: string; new: string };
		preferences?: Account['preferences'];
	}) {
		return this.#json<{ account: Account }>('/api/v1/auth/me', { method: 'PATCH', body });
	}

	listSignIns(signal?: AbortSignal) {
		return this.#json<{ sessions: AccountSession[] }>('/api/v1/auth/sessions', { signal });
	}

	/** Sign out everywhere: every session of this account but this one. */
	endOtherSignIns() {
		return this.#json<{ ended: number }>('/api/v1/auth/sessions', { method: 'DELETE' });
	}

	// --- accounts (spec 028 #12) -------------------------------------------
	//
	// The Server tab's second table, for owners and for the admin token. An
	// account is created without a password and reached by a link, so the one
	// response that carries a link is the one place it ever appears.

	listAccounts(signal?: AbortSignal) {
		return this.#json<{ accounts: AccountDetail[] }>('/api/v1/accounts', { signal });
	}

	createAccount(body: AccountInput) {
		return this.#json<Invitation>('/api/v1/accounts', { method: 'POST', body });
	}

	patchAccount(id: string, body: { name?: string; owner?: boolean; disabled?: boolean }) {
		return this.#json<{ account: AccountDetail }>(`/api/v1/accounts/${id}`, {
			method: 'PATCH',
			body
		});
	}

	/** A dry run until `confirm` echoes the email (spec 005 #8, Decision 12). */
	deleteAccount(id: string, confirm?: string) {
		return this.#json<AccountDeletion | void>(`/api/v1/accounts/${id}`, {
			method: 'DELETE',
			query: { confirm }
		});
	}

	/** A fresh link, which voids the previous one; this is also the reset. */
	inviteAccount(id: string) {
		return this.#json<{ invite_url: string; invite_expires_at: string; note?: string }>(
			`/api/v1/accounts/${id}/invite`,
			{ method: 'POST' }
		);
	}

	putMembership(id: string, projectID: string, role: MemberRole) {
		return this.#json<{ membership: { project_id: string; role: MemberRole } }>(
			`/api/v1/accounts/${id}/projects/${projectID}`,
			{ method: 'PUT', body: { role } }
		);
	}

	deleteMembership(id: string, projectID: string) {
		return this.#json<void>(`/api/v1/accounts/${id}/projects/${projectID}`, { method: 'DELETE' });
	}

	/** Who has a role in one project; owners are not rows (Decision 12). */
	listMembers(id: string, signal?: AbortSignal) {
		return this.#json<MemberList>(`/api/v1/projects/${id}/members`, { signal });
	}

	// --- project lifecycle (spec 007 #3, #4; now an owner's) ---------------
	//
	// What used to need the admin token in a browser. The token is gone from
	// the interface (Decision 14): an owner session reaches all of it, and a
	// credential the screens do not use is a credential the screens should not
	// hold.

	/** Every project, soft-deleted ones included with their purge dates. */
	listAllProjects(signal?: AbortSignal) {
		return this.#json<ProjectList>('/api/v1/projects', { query: { include: 'deleted' }, signal });
	}

	getProject(id: string, signal?: AbortSignal) {
		return this.#json<Project>(`/api/v1/projects/${id}`, { signal });
	}

	createProject(name: string) {
		return this.#json<Project & NewKey>('/api/v1/projects', { method: 'POST', body: { name } });
	}

	renameProject(id: string, name: string) {
		return this.#json<Project>(`/api/v1/projects/${id}`, { method: 'PATCH', body: { name } });
	}

	deleteProject(id: string, confirm?: string) {
		return this.#json<DryRun | Project>(`/api/v1/projects/${id}`, {
			method: 'DELETE',
			query: { confirm }
		});
	}

	restoreProject(id: string) {
		return this.#json<Project>(`/api/v1/projects/${id}/restore`, { method: 'POST' });
	}

	async #json<T>(path: string, options: Request = {}): Promise<T> {
		const response = await this.#fetch(path, options.query ?? {}, options);
		if (response.ok) {
			// A `204` is an answer with nothing in it — signing out, dropping a
			// membership, deleting a confirmed account — and `json()` on an
			// empty body throws.
			if (response.status === 204) return undefined as T;
			try {
				return (await response.json()) as T;
			} catch (cause) {
				// The clock covers the body too: headers that arrived and a
				// body that never finishes is the same silence as no headers.
				throw interrupted(cause) ?? cause;
			}
		}
		if (response.status === 401 && !options.anonymous) {
			// The cookie is gone, expired, or belongs to an account that was
			// disabled or deleted under this tab. Every screen reads project
			// data, so there is nothing to stay on: the person goes back to the
			// login form with where they were (Decision 13).
			auth.reject();
			throw new ApiError(401, 'the session has ended — sign in again');
		}
		throw await refusal(response);
	}

	async #fetch(path: string, query: Query, options: Request = {}): Promise<Response> {
		// The caller's signal and the clock, either of which ends the request;
		// `fetch` rejects with whichever one fired, and the reason's name says
		// which. Composed outside the `try` on purpose: a browser without
		// `AbortSignal.any` (spec 010 #10 states the floor) should fail here,
		// loudly, and not be reported as a server nobody can reach.
		const clock = options.clockUntilHeaders ? new AbortController() : null;
		const timer = clock
			? setTimeout(
					() => clock.abort(new DOMException('The operation timed out.', 'TimeoutError')),
					REQUEST_TIMEOUT_MS
				)
			: undefined;
		const signal = AbortSignal.any([
			clock?.signal ?? AbortSignal.timeout(REQUEST_TIMEOUT_MS),
			...(options.signal ? [options.signal] : [])
		]);
		let response: Response;
		try {
			response = await fetch(path + search(query), {
				method: options.method ?? 'GET',
				// The credential is the session cookie and nothing else
				// (Decision 4). `same-origin` rather than `include`: the
				// interface is served by the server it talks to, and a
				// cross-origin request from this app is a bug, not a feature.
				credentials: 'same-origin',
				// Never from the browser's cache (spec 021 #13). The prompt
				// reads carry `Cache-Control: max-age=60` for the SDKs that
				// poll them by label (spec 003 #14), and a screen that has
				// just moved a label would otherwise re-read the minute-old
				// answer and show the move as not having happened. Every other
				// endpoint sends no caching headers, so this changes nothing
				// for them and states what the data plane is: live.
				cache: options.cache ?? 'no-store',
				headers: {
					...projectHeader(path),
					...(options.body === undefined ? {} : { 'Content-Type': 'application/json' })
				},
				body: options.body === undefined ? undefined : JSON.stringify(options.body),
				signal
			});
		} catch (cause) {
			throw interrupted(cause) ?? new ApiError(0, 'cannot reach the server');
		} finally {
			clearTimeout(timer);
		}
		const version = response.headers.get(VERSION_HEADER);
		if (version) this.version = version;
		return response;
	}
}

/**
 * What a request cut short by its signal means to the caller, or `null` when
 * it was not the signal that ended it. The clock running out is the same
 * failure as a server nobody can reach, and the caller's to show — a load's
 * `failure`, a tick's `liveFailure` — not an abort, which lands nowhere (spec
 * 010 #8): the caller's own abort means the question went stale and no answer
 * to it is wanted, failure included, so it passes through as it is.
 *
 * Told apart by name rather than by class: `instanceof DOMException` is bound
 * to a realm, and a signal's reason need not come from this one — under jsdom
 * it does not.
 */
function interrupted(cause: unknown): unknown {
	const name = typeof cause === 'object' && cause !== null && 'name' in cause ? cause.name : '';
	if (name === 'TimeoutError') {
		// Marked, because for a request the server runs to completion — an
		// erasure (spec 035 #14) — "no answer in time" is not a failure.
		return new ApiError(0, 'the server did not answer in time', { timed_out: true });
	}
	if (name === 'AbortError') return cause;
	return null;
}

/**
 * The routes that carry their project in the path, or have none at all: a
 * session sends no `X-Tracepad-Project` on any of them (Decision 19), and the
 * server ignores it where it is meaningless.
 */
const CARRY_THEIR_OWN = ['/api/v1/projects', '/api/v1/auth', '/api/v1/accounts', '/api/v1/setup'];

/**
 * Which project this request is about (Decision 6). A key was its own project
 * and a session is not, so the interface says so in one place — here — rather
 * than in every call site; since spec 029 the id is the page URL's, and no
 * call site noticed.
 *
 * Read untracked: a request's project is a fact at the moment it goes out,
 * not a subscription. Most reads are started inside an `$effect`, before its
 * first `await`, and the id comes off `page.params` — which is a new object
 * on every navigation. Tracked, every such effect re-ran on every `?obs=`
 * and `?peek=`, and a trace re-read itself on each arrow key.
 */
function projectHeader(path: string): Record<string, string> {
	if (CARRY_THEIR_OWN.some((prefix) => path.startsWith(prefix))) return {};
	const id = untrack(() => project.id);
	return id ? { 'X-Tracepad-Project': id } : {};
}

/** Everything one request can vary. */
type Request = {
	method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
	query?: Query;
	body?: unknown;
	/**
	 * Whether a `401` is this request's own answer rather than the end of the
	 * session. The login form's refusal and the guard's "nobody is signed in"
	 * are both 401s that belong to the caller; everything else is a session
	 * that has ended, and only that sends the person to the login form.
	 */
	anonymous?: boolean;
	signal?: AbortSignal;
	/** Only a content-addressed read overrides `no-store` (spec 041 #7). */
	cache?: RequestCache;
	/**
	 * Whether the clock stops once the answer's headers are in. A media body
	 * may be tens of megabytes on a slow link: the clock is there for a server
	 * that does not answer, not for a body that takes its time.
	 */
	clockUntilHeaders?: boolean;
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
