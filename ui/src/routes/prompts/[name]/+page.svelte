<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import GitCompare from '@lucide/svelte/icons/git-compare';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Plus from '@lucide/svelte/icons/plus';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api, type Prompt, type PromptVersionRow } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PaginationBar from '$lib/components/PaginationBar.svelte';
	import DeletePromptDialog from '$lib/components/prompts/DeletePromptDialog.svelte';
	import DiffView from '$lib/components/prompts/DiffView.svelte';
	import LabelChip from '$lib/components/prompts/LabelChip.svelte';
	import VersionView from '$lib/components/prompts/VersionView.svelte';
	import { timestamp } from '$lib/format';
	import { asPage, Listing, UrlSpot } from '$lib/listing.svelte';
	import { diffParam, orderLabelEntries, orderLabels, readDiff } from '$lib/prompts';

	// One prompt (spec 021 #2): the versions down the left as the shared
	// listing, and the version the URL names on the right — the two are one
	// thing seen at two depths, so a route per version would be a page with one
	// paragraph. `?diff=A..B` replaces the right-hand side with the server's
	// patch (#3); nothing else about the page changes, because a diff is a way
	// of reading the same prompt.

	const name = $derived(page.params.name ?? '');
	const asked = $derived(page.url.searchParams.get('version'));
	const diff = $derived(readDiff(page.url.searchParams.get('diff')));

	/** Every label of the name and where it points, from the version listing (#12). */
	let named = $state.raw<Record<string, number>>({});

	const versions = new Listing<PromptVersionRow & { id: string }>({
		key: () => name,
		spot: new UrlSpot(),
		count: false,
		read: async (at, counting, signal) => {
			const answer = await api.listPromptVersions(name, asPage(at, counting), signal);
			named = answer.labels;
			return { ...answer, rows: answer.versions.map((row) => ({ ...row, id: String(row.version) })) };
		},
		failed: 'Failed to read the versions.'
	});

	let prompt = $state.raw<Prompt | null>(null);
	let missing = $state<string | null>(null);
	let deleting = $state(false);

	$effect(() => {
		const controller = new AbortController();
		void load(name, asked, controller.signal);
		return () => controller.abort();
	});

	/** Without a signal: a refresh after a label move, which nothing aborts. */
	async function load(wanted: string, version: string | null, signal?: AbortSignal) {
		missing = null;
		try {
			const at = version === null ? {} : { version: Number(version) };
			const answer = await api.getPrompt(wanted, at, signal);
			if (!signal?.aborted) prompt = answer;
		} catch (cause) {
			if (signal?.aborted) return;
			prompt = null;
			// A `?version=` that is not there is the page's own not-found
			// state, with the versions list still on screen (edge cases).
			missing = cause instanceof ApiError ? cause.message : 'Failed to read the prompt.';
		}
	}

	/** After a label move: the version and the name's map both changed. */
	function refresh() {
		void load(name, asked);
		versions.reload();
	}

	const here = $derived(`/prompts/${encodeURIComponent(name)}`);
	/** The version on screen; the diff's right-hand side when one is open. */
	const shown = $derived(prompt?.version ?? (diff ? diff.to : null));

	/** A link to a version of this name, keeping the page the reader is on. */
	function at(version: number | null, comparing: string | null = null): string {
		const params = new URLSearchParams(page.url.searchParams);
		params.delete('version');
		params.delete('diff');
		if (version !== null) params.set('version', String(version));
		if (comparing !== null) params.set('diff', comparing);
		const search = params.toString();
		return `${here}${search ? `?${search}` : ''}`;
	}

	/** Opening the diff: this version against the one before it (#3). */
	function openDiff() {
		const to = shown ?? 1;
		goto(at(null, diffParam(Math.max(1, to - 1), to)));
	}

	function pick(side: 'from' | 'to', raw: string) {
		const value = Number(raw);
		if (!diff || !Number.isInteger(value) || value < 1) return;
		goto(at(null, diffParam(side === 'from' ? value : diff.from, side === 'to' ? value : diff.to)));
	}

	/** The highest version there is, for the diff inputs' ceiling. */
	const latest = $derived(versions.rows.length > 0 ? Math.max(...versions.rows.map((r) => r.version)) : undefined);
	/** A name with one version has nothing to compare it with (#9). */
	const comparable = $derived(versions.rows.length > 1 || !versions.newest);

	const numberField =
		'border-border bg-canvas text-fg w-16 rounded-md border px-1.5 py-1 text-sm tabular-nums';
</script>

<svelte:head><title>{name} · Prompts · Tracepad</title></svelte:head>

<PageHeader title={name}>
	{#snippet meta()}
		<a href="/prompts" class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			Prompts
		</a>
		{#if prompt}
			<span class="border-border hidden rounded-full border px-2 py-0.5 text-xs sm:inline">
				{prompt.type}
			</span>
		{/if}
		<div class="hidden flex-wrap gap-1 md:flex">
			{#each orderLabelEntries(named) as [label, version] (label)}
				<a href={at(version)} class="hover:opacity-80"><LabelChip {label} {version} /></a>
			{/each}
		</div>
	{/snippet}
	{#snippet actions()}
		<Button
			variant="primary"
			aria-label="New version"
			onclick={() =>
				goto(`${here}/versions/new${shown === null ? '' : `?from=${shown}`}`)}
		>
			<Plus class="size-4" />
			<span class="hidden sm:inline">New version</span>
		</Button>
		{#if diff}
			<Button aria-label="Close the diff" onclick={() => goto(at(shown))}>
				<GitCompare class="size-4" />
				<span class="hidden sm:inline">Close diff</span>
			</Button>
		{:else}
			<Button aria-label="Diff" disabled={!comparable} onclick={openDiff}>
				<GitCompare class="size-4" />
				<span class="hidden sm:inline">Diff</span>
			</Button>
		{/if}
		<Button aria-label="Delete prompt" onclick={() => (deleting = true)}>
			<Trash2 class="size-4" />
			<span class="hidden sm:inline">Delete</span>
		</Button>
	{/snippet}
</PageHeader>

<DeletePromptDialog open={deleting} {name} onclose={() => (deleting = false)} />

{#if versions.problem}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{versions.problem}
	</p>
{/if}

<div class="flex min-h-0 flex-1 flex-col md:flex-row">
	<!-- The versions. On a phone this stacks above the view rather than beside
	     it, capped so that the body a reader came for is not below the fold. -->
	<div
		class="border-border flex max-h-64 shrink-0 flex-col border-b md:h-auto md:max-h-none
			md:w-72 md:border-r md:border-b-0"
	>
		<div class="min-h-0 flex-1 overflow-auto">
			<table class="w-full border-collapse text-left">
				<caption class="sr-only">Versions</caption>
				<tbody>
					{#each versions.rows as row (row.version)}
						<tr
							class={[
								'border-border border-b transition-colors duration-100',
								row.version === shown ? 'bg-accent-soft' : 'hover:bg-raised'
							]}
						>
							<td class="px-3 py-1.5">
								<a href={at(row.version)} class="block">
									<span class="flex items-baseline gap-2">
										<span class="font-medium tabular-nums">v{row.version}</span>
										<span class="text-subtle font-mono text-xs tabular-nums">
											{timestamp(row.created_at)}
										</span>
									</span>
									{#if row.commit_message}
										<span class="text-muted mt-0.5 block truncate text-xs">
											{row.commit_message}
										</span>
									{/if}
									{#if row.labels.length > 0}
										<span class="mt-1 flex flex-wrap gap-1">
											{#each orderLabels(row.labels) as label (label)}
												<LabelChip {label} />
											{/each}
										</span>
									{/if}
								</a>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
		<PaginationBar {...versions.bar} noun="version" />
	</div>

	<div class="min-h-0 min-w-0 flex-1 overflow-auto">
		{#if diff}
			<div class="border-border flex flex-wrap items-center gap-2 border-b px-4 py-2">
				<label class="text-subtle flex items-center gap-1.5 text-xs">
					From
					<!-- Numbers, not dropdowns: a name edited in CI has hundreds
					     of versions (edge cases). -->
					<input
						id="diff-from"
						name="from"
						type="number"
						min="1"
						max={latest}
						value={diff.from}
						onchange={(event) => pick('from', event.currentTarget.value)}
						aria-label="Diff from version"
						class={numberField}
					/>
				</label>
				<label class="text-subtle flex items-center gap-1.5 text-xs">
					To
					<input
						id="diff-to"
						name="to"
						type="number"
						min="1"
						max={latest}
						value={diff.to}
						onchange={(event) => pick('to', event.currentTarget.value)}
						aria-label="Diff to version"
						class={numberField}
					/>
				</label>
			</div>
			<DiffView {name} from={diff.from} to={diff.to} />
		{:else if missing}
			<div class="flex items-start gap-2 p-4">
				<TriangleAlert class="text-danger mt-0.5 size-4 shrink-0" />
				<div>
					<p role="alert" class="text-danger text-sm">{missing}</p>
					<a class="text-accent mt-1 inline-block text-sm underline underline-offset-2" href={at(null)}>
						Read the latest version
					</a>
				</div>
			</div>
		{:else if prompt}
			<VersionView {prompt} {named} onchanged={refresh} />
		{:else}
			<p class="text-subtle flex items-center gap-2 p-4 text-sm">
				<LoaderCircle class="size-4 animate-spin" />
				Reading the prompt
			</p>
		{/if}
	</div>
</div>

{#if !comparable && !diff}
	<p class="text-subtle border-border border-t px-4 py-2 text-xs">
		No diff yet — a prompt is compared with its own earlier versions, and this name has one.
	</p>
{/if}
