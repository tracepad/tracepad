<script lang="ts">
	import Inbox from '@lucide/svelte/icons/inbox';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type Dataset, type DatasetItem } from '$lib/api/client.svelte';
	import { short } from '$lib/evals';
	import { asPage, Listing, UrlSpot, Walk } from '$lib/listing.svelte';
	import { peekSearch, readPeek } from '$lib/peek';
	import CopyButton from '../CopyButton.svelte';
	import ListingShell from '../ListingShell.svelte';
	import PeekPanel from '../PeekPanel.svelte';
	import ItemDetail from './ItemDetail.svelte';
	import ItemTable from './ItemTable.svelte';

	// The items tab of a dataset (spec 016 #4): the items at the version in
	// force over the shared listing (#3), walked oldest first in `seq` order,
	// and a row's peek — the item whole.

	let {
		dataset,
		version,
		onchanged
	}: {
		dataset: Dataset;
		version: number;
		/** A write landed: the envelope above this tab is a version out of date. */
		onchanged?: () => void;
	} = $props();

	/** Whether the head is in force, which is where a write would land (#4). */
	const head = $derived(version === dataset.version);

	const listing = new Listing<DatasetItem>({
		key: () => `${dataset.name}|${version}`,
		spot: new UrlSpot(),
		count: false,
		read: async (at, counting, signal) => {
			const answer = await api.listItems(dataset.name, version, asPage(at, counting), signal);
			return { ...answer, rows: answer.items };
		},
		failed: 'Failed to read the items.'
	});

	const peekID = $derived(readPeek(page.url.searchParams).peek);

	const walk = new Walk(listing, {
		key: (row) => String(row.seq).padStart(12, '0'),
		peekID: () => peekID,
		showing: () => null,
		open: peek,
		ascending: true
	});

	/** Opening pushes one entry; moving between rows replaces it (spec 008 #6). */
	function peek(id: string | null) {
		const search = peekSearch(page.url.searchParams, { peek: id });
		goto(`${page.url.pathname}${search}`, {
			replaceState: id === null || peekID !== null,
			keepFocus: true,
			noScroll: true
		});
	}

	// The exact count is on the dataset at the head; an older version has no
	// number of its own, and the bar counts the page (spec 010, divergence 6).
	const total = $derived(head ? { value: dataset.item_count, capped: false } : null);

	/** An archive is a new version of the dataset: the tab and its header both move. */
	function archived() {
		peek(null);
		listing.reload();
		onchanged?.();
	}

	const push = $derived(`tracepad datasets push ${dataset.name} --file cases.jsonl`);
</script>

<ListingShell {listing} noun="item" {total} back="first">
	{#snippet table()}
		<ItemTable
			rows={listing.rows}
			onopen={peek}
			href={(id) => peekSearch(page.url.searchParams, { peek: id })}
			selectedID={peekID}
		/>
	{/snippet}
	{#snippet empty()}
		<div class="flex flex-1 items-start justify-center overflow-auto p-8">
			<div class="max-w-lg">
				<h2 class="flex items-center gap-2 font-medium">
					<Inbox class="text-subtle size-4" />
					No items at version {version}
				</h2>
				<!-- The empty state teaches the CLI (#15): a case file is one push
				     away, and a batch is one version tick. -->
				<p class="text-muted mt-1">
					Push a <code class="font-mono">.jsonl</code> of cases — one per line, each with an
					<code class="font-mono">input</code> and optionally an
					<code class="font-mono">expected_output</code> — and the dataset moves to its next version.
				</p>
				<div class="border-border bg-surface mt-3 flex items-start gap-2 rounded-md border p-3">
					<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{push}</pre>
					<CopyButton text={push} label="Copy the push command" />
				</div>
			</div>
		</div>
	{/snippet}
</ListingShell>

{#if peekID}
	<PeekPanel
		label="Item"
		onclose={() => peek(null)}
		onprev={() => walk.step(-1)}
		onnext={() => walk.step(1)}
		hasPrev={walk.hasPrev}
		hasNext={walk.hasNext}
		fullHref={head
			? `/datasets/${encodeURIComponent(dataset.name)}/items/${encodeURIComponent(peekID)}/edit`
			: `/datasets/${encodeURIComponent(dataset.name)}?version=${version}&peek=${peekID}`}
		fullLabel={head ? 'Open this item in the editor' : 'Link to this item'}
	>
		{#snippet title()}
			<h2 class="shrink-0 text-lg font-semibold tracking-tight">Item {short(peekID)}</h2>
		{/snippet}
		{#snippet meta()}
			<span class="hidden truncate font-mono md:inline">{peekID}</span>
			<CopyButton text={peekID} label="Copy the item id" />
		{/snippet}
		<ItemDetail dataset={dataset.name} id={peekID} {version} writable={head} onarchived={archived} />
	</PeekPanel>
{/if}
