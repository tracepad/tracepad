<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { ApiError, api, type DatasetItem, type ItemVersions } from '$lib/api/client.svelte';
	import { ABSENT, timestamp } from '$lib/format';
	import JsonView from '../json/JsonView.svelte';

	// One dataset item, whole: the three bodies as documents (spec 015), where
	// the case came from when it was cut from a trace (spec 014 #4), and every
	// row of its history (spec 014 #5). Read from the item endpoint rather than
	// from the listing's row, so a deep link and a walked-to row show the same
	// thing — and so the body is the item's, not a preview of it.

	let {
		dataset,
		id,
		version
	}: {
		dataset: string;
		id: string;
		/** The version the page is reading at; the current one when absent. */
		version?: number;
	} = $props();

	let item = $state.raw<DatasetItem | null>(null);
	let history = $state.raw<ItemVersions | null>(null);
	let loading = $state(true);
	let failure = $state<string | null>(null);

	$effect(() => {
		const controller = new AbortController();
		void load(dataset, id, version, controller.signal);
		return () => controller.abort();
	});

	async function load(name: string, wanted: string, at: number | undefined, signal: AbortSignal) {
		loading = true;
		failure = null;
		item = history = null;
		try {
			const [row, rows] = await Promise.all([
				api.getItem(name, wanted, at, signal),
				api.listItemVersions(name, wanted, signal)
			]);
			if (signal.aborted) return;
			item = row;
			history = rows;
		} catch (cause) {
			if (signal.aborted) return;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the item.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	// An item reached from an older version may be gone from the head: its
	// newest row is an archive (edge cases). The note says so, and at which
	// version, because "not in the current dataset" is the fact an editor
	// would otherwise have to discover by trying.
	const archivedAt = $derived.by(() => {
		const newest = history?.versions[0];
		return newest?.archived ? newest.version : null;
	});
</script>

{#if loading}
	<div class="text-subtle flex flex-1 items-center justify-center gap-2">
		<LoaderCircle class="size-4 animate-spin" />
		Loading the item
	</div>
{:else if failure}
	<div class="flex flex-1 items-start justify-center p-8">
		<p role="alert" class="text-danger flex max-w-md items-start gap-2">
			<TriangleAlert class="mt-0.5 size-4 shrink-0" />
			{failure}
		</p>
	</div>
{:else if item}
	<div class="min-h-0 flex-1 overflow-auto">
		<dl class="border-border flex flex-wrap gap-x-8 gap-y-2 border-b px-4 py-3 text-sm">
			<div>
				<dt class="text-subtle text-xs">Position</dt>
				<dd class="tabular-nums">{item.seq}</dd>
			</div>
			<div>
				<dt class="text-subtle text-xs">Row version</dt>
				<dd class="tabular-nums">v{item.version}</dd>
			</div>
			<div>
				<dt class="text-subtle text-xs">Written</dt>
				<dd class="tabular-nums">{timestamp(item.created_at)}</dd>
			</div>
			{#if item.source_trace_id}
				<div class="min-w-0">
					<dt class="text-subtle text-xs">Cut from</dt>
					<dd class="truncate font-mono text-xs">
						<!-- Strings, not references (spec 014 #4): the trace may be gone,
						     and the link is offered rather than promised. -->
						<a
							class="text-accent underline underline-offset-2"
							href="/traces/{encodeURIComponent(item.source_trace_id)}{item.source_observation_id
								? `?obs=${encodeURIComponent(item.source_observation_id)}`
								: ''}"
						>
							{item.source_trace_id}{item.source_observation_id
								? ` · ${item.source_observation_id}`
								: ''}
						</a>
					</dd>
				</div>
			{/if}
		</dl>

		{#if archivedAt !== null}
			<p class="text-warn border-border border-b px-4 py-2 text-sm">
				Archived at version {archivedAt}: this item is not in the current dataset.
			</p>
		{/if}

		{#each [['Input', item.input], ['Expected output', item.expected_output], ['Metadata', item.metadata]] as [label, value] (label)}
			<section class="border-border border-b px-4 py-3">
				<h3 class="text-muted mb-2 text-xs font-medium tracking-wide uppercase">{label}</h3>
				{#if value === null || value === undefined}
					<p class="text-subtle">{ABSENT}</p>
				{:else}
					<JsonView {value} label={String(label)} />
				{/if}
			</section>
		{/each}

		{#if history}
			<section class="px-4 py-3">
				<h3 class="text-muted mb-2 text-xs font-medium tracking-wide uppercase">Versions</h3>
				<!-- Every row of the item, newest first: an edit is a row and an
				     archive is a row (spec 014 #5). Each links to the dataset at
				     that version, where the row can be read in its context. -->
				<ul class="text-sm">
					{#each history.versions as row (row.version)}
						<li class="flex items-center gap-2 py-0.5">
							<a
								class="text-accent tabular-nums underline underline-offset-2"
								href="/datasets/{encodeURIComponent(dataset)}?tab=items&version={row.version}&peek={id}"
							>
								v{row.version}
							</a>
							<span class="text-subtle font-mono text-xs tabular-nums">{timestamp(row.created_at)}</span>
							{#if row.archived}
								<span class="text-warn text-xs">archived</span>
							{/if}
							{#if row.version === item.version}
								<span class="text-subtle text-xs">(shown)</span>
							{/if}
						</li>
					{/each}
				</ul>
			</section>
		{/if}
	</div>
{/if}
