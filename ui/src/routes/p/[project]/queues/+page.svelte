<script lang="ts">
	import ClipboardCheck from '@lucide/svelte/icons/clipboard-check';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { ApiError, api, type AnnotationQueue, type ScoreConfig } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import NewQueueDialog from '$lib/components/queues/NewQueueDialog.svelte';
	import ProgressBar from '$lib/components/queues/ProgressBar.svelte';
	import { Fold } from '$lib/fold.svelte';
	import { count } from '$lib/format';
	import { href, project } from '$lib/project.svelte';

	// The review programmes of the project (spec 024 #10), whole and in name
	// order over `GET /api/v1/queues`: a project has as many queues as it has
	// review programmes, so there is nothing to page.

	let queues = $state.raw<AnnotationQueue[] | null>(null);
	let configs = $state.raw<ScoreConfig[]>([]);
	let failure = $state<string | null>(null);
	/** Bumped by a write, which is what re-reads the list. */
	let generation = $state(0);

	$effect(() => {
		void generation;
		const controller = new AbortController();
		Promise.all([api.listQueues(controller.signal), api.listScoreConfigs(controller.signal)])
			.then(([listing, declared]) => {
				if (controller.signal.aborted) return;
				queues = listing.queues;
				configs = declared.configs;
				failure = null;
			})
			.catch((cause: unknown) => {
				if (controller.signal.aborted) return;
				failure = cause instanceof ApiError ? cause.message : 'Failed to read the queues.';
			});
		return () => controller.abort();
	});

	let creating = $state(false);

	// The whole loop in four lines (spec 016 #15's habit): somebody landing
	// here with no queue is usually about to script one.
	const loop = [
		'tracepad queues put weekly-review --config accuracy --config tone',
		'tracepad queues add weekly-review --from-traces --error --since 168h',
		'tracepad queues next weekly-review --annotator ada',
		'# score the trace, then: tracepad queues complete weekly-review <item> --annotator ada'
	].join('\n');

	const cell = 'truncate px-3 py-1.5';

	// In a box narrower than the table a row is the queue's name and how far
	// along it is; the description and the scores it asks for fold under the
	// name (spec 006 #22). The number is the unfolded table's width and its
	// `min-width`.
	const fold = new Fold(736);
	const narrow = $derived(fold.narrow);
</script>

<svelte:head><title>Queues · Tracepad</title></svelte:head>

<PageHeader title="Queues">
	{#snippet meta()}
		{#if queues === null && !failure}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else if queues}
			<span class="tabular-nums">{count(queues.length)}</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<!-- Working a queue is a viewer's job; making one is not (Decision 3). -->
		{#if project.editor}
			<Button variant="primary" onclick={() => (creating = true)}>New queue</Button>
		{/if}
	{/snippet}
</PageHeader>

<NewQueueDialog open={creating} {configs} onclose={() => (creating = false)} />

{#if failure}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{failure}
	</p>
{:else if queues && queues.length > 0}
	<div bind:clientWidth={fold.box} class="min-h-0 flex-1 overflow-auto">
		<table class="w-full border-collapse text-left" style:min-width={fold.min}>
			<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
				<tr class="border-border border-b">
					<th scope="col" class={['px-3 py-2 font-medium', !narrow && 'w-56']}>Name</th>
					{#if !narrow}
						<th scope="col" class="px-3 py-2 font-medium">Description</th>
						<th scope="col" class="w-64 px-3 py-2 font-medium">Scores</th>
					{/if}
					<th scope="col" class={['px-3 py-2 font-medium', narrow ? 'w-32' : 'w-52']}>Progress</th>
				</tr>
			</thead>
			<tbody>
				{#each queues as queue (queue.name)}
					<tr class="border-border hover:bg-raised border-b transition-colors duration-100">
						{#if narrow}
							<td class="max-w-0 px-3 py-1.5">
								<a
									href={href(`/queues/${encodeURIComponent(queue.name)}`)}
									class="hover:text-accent block truncate font-medium"
								>
									{queue.name}
								</a>
								{#if queue.description}
									<div class="text-muted truncate text-xs">{queue.description}</div>
								{/if}
								<div class="mt-0.5 flex flex-wrap gap-1">
									{#each queue.score_configs as name (name)}
										<span class="border-border bg-surface text-muted rounded-md border px-1 text-xs">
											{name}
										</span>
									{/each}
								</div>
							</td>
						{:else}
							<td class="{cell} font-medium">
								<a href={href(`/queues/${encodeURIComponent(queue.name)}`)} class="hover:text-accent">
									{queue.name}
								</a>
							</td>
							<td class="text-muted {cell}">{queue.description || '—'}</td>
							<td class="px-3 py-1.5">
								<div class="flex flex-wrap gap-1">
									{#each queue.score_configs as name (name)}
										<span
											class="border-border bg-surface text-muted rounded-md border px-1.5 py-0.5 text-xs"
										>
											{name}
										</span>
									{/each}
								</div>
							</td>
						{/if}
						<td class="px-3 py-1.5"><ProgressBar {queue} stacked={narrow} /></td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{:else if queues}
	<div class="flex flex-1 items-start justify-center overflow-auto p-8">
		<div class="max-w-2xl">
			<h2 class="flex items-center gap-2 font-medium">
				<ClipboardCheck class="text-subtle size-4" />
				No queues yet
			</h2>
			<p class="text-muted mt-1">
				A queue is a list of traces somebody has decided deserve a human verdict, and the scores
				that verdict is made of. Fill it from a trace, from a filtered listing, or from a script:
			</p>
			<div class="border-border bg-surface mt-3 flex items-start gap-2 rounded-md border p-3">
				<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{loop}</pre>
				<CopyButton text={() => loop} label="Copy the loop" />
			</div>
			<a
				class="text-accent mt-3 inline-block underline underline-offset-2"
				href="https://github.com/tracepad/tracepad/blob/main/docs/annotation.md"
				target="_blank"
				rel="noreferrer"
			>
				Annotation queues
			</a>
		</div>
	</div>
{:else}
	<div class="flex-1"></div>
{/if}
