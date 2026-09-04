<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Ruler from '@lucide/svelte/icons/ruler';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { ApiError, api, type ScoreConfig } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import ScoreConfigDialog from '$lib/components/evals/ScoreConfigDialog.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import { trim } from '$lib/evals';
	import { count } from '$lib/format';

	// The score configs of the project, whole (spec 014 #25): what each score
	// name means — its type, which way is better, what it admits. A form writes
	// one (spec 016 #9), because a config is a declaration a person makes once
	// and the endpoint takes the whole of it, so the same form creates and
	// edits.

	let configs = $state.raw<ScoreConfig[] | null>(null);
	let failure = $state<string | null>(null);
	/** Bumped by a write, which is what re-reads the list. */
	let generation = $state(0);

	$effect(() => {
		void generation;
		const controller = new AbortController();
		api
			.listScoreConfigs(controller.signal)
			.then((answer) => {
				if (controller.signal.aborted) return;
				configs = answer.configs;
				// A read that succeeded clears the last one's failure: the list
				// reloads after every write now, and a banner that outlived the
				// failure would stand over a table that is already correct
				// (found in review of this PR).
				failure = null;
			})
			.catch((cause: unknown) => {
				if (controller.signal.aborted) return;
				failure = cause instanceof ApiError ? cause.message : 'Failed to read the score configs.';
			});
		return () => controller.abort();
	});

	// The dialog serves both writes; `editing` is which row it is about, and
	// `null` with `writing` on is a new name.
	let writing = $state(false);
	let editing = $state.raw<ScoreConfig | null>(null);
	let deleting = $state.raw<ScoreConfig | null>(null);

	function open(config: ScoreConfig | null) {
		editing = config;
		writing = true;
	}

	/** What a config admits: the bounds of a number, or the words a category may be. */
	function admits(config: ScoreConfig): string {
		if (config.categories?.length) return config.categories.join(', ');
		const { min, max } = config;
		if (min != null && max != null) return `${trim(min)} … ${trim(max)}`;
		if (min != null) return `at least ${trim(min)}`;
		if (max != null) return `at most ${trim(max)}`;
		return '—';
	}

	const push =
		'tracepad score-configs push accuracy --file accuracy.json\n' +
		'# accuracy.json: {"data_type": "numeric", "direction": "higher", "min": 0, "max": 1}';

	const cell = 'truncate px-3 py-1.5';
</script>

<svelte:head><title>Score configs · Tracepad</title></svelte:head>

<PageHeader title="Score configs">
	{#snippet meta()}
		{#if configs === null && !failure}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else if configs}
			<span class="tabular-nums">{count(configs.length)}</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button variant="primary" onclick={() => open(null)}>New score config</Button>
	{/snippet}
</PageHeader>

<ScoreConfigDialog
	open={writing}
	config={editing}
	onclose={() => (writing = false)}
	onsaved={() => generation++}
/>

<ConfirmDialog
	open={deleting !== null}
	title="Remove {deleting?.name ?? ''}?"
	description="The binding goes; the scores already posted under this name stay exactly as they are.
		Nothing validates the name afterwards, so a later score may be of any type."
	confirmLabel="Remove the config"
	onconfirm={async () => {
		if (deleting) await api.deleteScoreConfig(deleting.name);
		generation++;
	}}
	onclose={() => (deleting = null)}
/>

{#if failure}
	<p role="alert" class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2">
		<TriangleAlert class="size-4 shrink-0" />
		{failure}
	</p>
{:else if configs && configs.length > 0}
	<div class="min-h-0 flex-1 overflow-auto">
		<table class="w-full min-w-2xl border-collapse text-left">
			<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
				<tr class="border-border border-b">
					<th scope="col" class="w-48 px-3 py-2 font-medium">Name</th>
					<th scope="col" class="w-28 px-3 py-2 font-medium">Type</th>
					<th scope="col" class="w-28 px-3 py-2 font-medium">Direction</th>
					<th scope="col" class="w-64 px-3 py-2 font-medium">Admits</th>
					<th scope="col" class="px-3 py-2 font-medium">Description</th>
					<th scope="col" class="w-40 px-3 py-2"><span class="sr-only">Actions</span></th>
				</tr>
			</thead>
			<tbody>
				{#each configs as config (config.name)}
					<tr class="border-border hover:bg-raised border-b transition-colors duration-100">
						<td class="{cell} font-medium">{config.name}</td>
						<td class="text-muted {cell}">{config.data_type}</td>
						<td class="text-muted {cell}">{config.direction ?? '—'}</td>
						<td class="text-muted {cell} tabular-nums" title={admits(config)}>{admits(config)}</td>
						<td class="text-muted {cell}">{config.description ?? '—'}</td>
						<td class="px-3 py-1.5">
							<div class="flex justify-end gap-1.5">
								<Button onclick={() => open(config)}>Edit</Button>
								<Button variant="ghost" onclick={() => (deleting = config)}>Remove</Button>
							</div>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{:else if configs}
	<div class="flex flex-1 items-start justify-center overflow-auto p-8">
		<div class="max-w-lg">
			<h2 class="flex items-center gap-2 font-medium">
				<Ruler class="text-subtle size-4" />
				No score configs yet
			</h2>
			<p class="text-muted mt-1">
				A config pins what a score's name means — its type, which direction is better, its range
				— so that names and scales do not drift between harnesses, and so a comparison can say
				<em>improved</em> rather than <em>changed</em>. Declare them at the top of the harness:
			</p>
			<div class="border-border bg-surface mt-3 flex items-start gap-2 rounded-md border p-3">
				<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{push}</pre>
				<CopyButton text={push} label="Copy the push command" />
			</div>
		</div>
	</div>
{:else}
	<div class="flex-1"></div>
{/if}
