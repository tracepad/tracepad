<script lang="ts">
	import ScrollText from '@lucide/svelte/icons/scroll-text';
	import { goto } from '$app/navigation';
	import { api, type PromptRow } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import Folded from '$lib/components/Folded.svelte';
	import ListingCount from '$lib/components/ListingCount.svelte';
	import ListingShell from '$lib/components/ListingShell.svelte';
	import LabelChip from '$lib/components/prompts/LabelChip.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import { Fold } from '$lib/fold.svelte';
	import { timestamp } from '$lib/format';
	import { asPage, Listing, UrlSpot } from '$lib/listing.svelte';
	import { orderLabelEntries } from '$lib/prompts';
	import { href, project } from '$lib/project.svelte';

	// The project's prompts over `GET /api/v1/prompts` (spec 021, Application
	// contract): the shared listing with no count, because the endpoint offers
	// none — prompts are as many as somebody published.

	const listing = new Listing<PromptRow & { id: string }>({
		key: () => 'prompts',
		spot: new UrlSpot(),
		count: false,
		read: async (at, counting, signal) => {
			const answer = await api.listPrompts(asPage(at, counting), signal);
			return { ...answer, rows: answer.prompts.map((row) => ({ ...row, id: row.name })) };
		},
		failed: 'Failed to read the prompts.'
	});

	// The two lines that put a prompt here and read it back (#9): the person
	// on this empty screen is the one about to write the client.
	const publish = 'tracepad prompts push support --file prompt.json --label production';
	const fetch = 'tracepad.prompt("support", label="production")';

	const cell = 'max-w-0 truncate px-3 py-1.5';

	// In a box narrower than the table a row is the prompt's name and its
	// labels — which version is live is what a reader here is after — and the
	// type, the latest version and the date fold under the name (spec 006
	// #22). The number is the unfolded table's width and its `min-width`.
	const fold = new Fold(672);
	const narrow = $derived(fold.narrow);
</script>

<svelte:head><title>Prompts · Tracepad</title></svelte:head>

<PageHeader title="Prompts">
	{#snippet meta()}
		<ListingCount {listing} />
	{/snippet}
	{#snippet actions()}
		<!-- The listing is every role's; writing a prompt is an editor's
		     (spec 028 #15). -->
		{#if project.editor}
			<Button variant="primary" onclick={() => goto(href('/prompts/new'))}>New prompt</Button>
		{/if}
	{/snippet}
</PageHeader>

<ListingShell {listing} noun="prompt" back="first">
	{#snippet table()}
		<div bind:contentRect={fold.rect} class="min-h-0 flex-1 overflow-auto">
			<table class="w-full border-collapse text-left" style:min-width={fold.min}>
				<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
					<tr class="border-border border-b">
						<th scope="col" class={['px-3 py-2 font-medium', narrow ? 'w-1/2' : 'w-64']}>Name</th>
						{#if !narrow}
							<th scope="col" class="w-20 px-3 py-2 font-medium">Type</th>
							<th scope="col" class="w-20 px-3 py-2 text-right font-medium">Latest</th>
						{/if}
						<th scope="col" class="px-3 py-2 font-medium">Labels</th>
						{#if !narrow}
							<th scope="col" class="w-44 px-3 py-2 font-medium">Updated</th>
						{/if}
					</tr>
				</thead>
				<tbody>
					{#each listing.rows as row (row.name)}
						<tr class="border-border hover:bg-raised border-b transition-colors duration-100">
							<td class={[cell, 'font-medium']} title={row.name}>
								<a href={href(`/prompts/${encodeURIComponent(row.name)}`)} class="hover:text-accent">
									{row.name}
								</a>
								{#if narrow}
									<div class="text-muted text-xs font-normal whitespace-normal tabular-nums">
										<Folded
											values={[
												row.type,
												`v${row.latest_version}`,
												`updated ${timestamp(row.updated_at)}`
											]}
										/>
									</div>
								{/if}
							</td>
							{#if !narrow}
								<td class="text-muted {cell}">{row.type}</td>
								<td class="text-muted px-3 py-1.5 text-right tabular-nums">v{row.latest_version}</td>
							{/if}
							<td class="px-3 py-1.5">
								<div class="flex flex-wrap gap-1">
									{#each orderLabelEntries(row.labels) as [label, version] (label)}
										<LabelChip {label} {version} />
									{/each}
								</div>
							</td>
							{#if !narrow}
								<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
									{timestamp(row.updated_at)}
								</td>
							{/if}
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/snippet}
	{#snippet empty()}
		<div class="flex flex-1 items-start justify-center overflow-auto p-8">
			<div class="max-w-2xl">
				<h2 class="flex items-center gap-2 font-medium">
					<ScrollText class="text-subtle size-4" />
					No prompts yet
				</h2>
				<p class="text-muted mt-1">
					A prompt is versioned here and labelled: versions are append-only, and promoting one to
					production — or rolling it back — is a label move rather than a release. Publish one from
					the command line, or with <em>New prompt</em> above:
				</p>
				{#snippet line(code: string, caption: string)}
					<p class="text-subtle mt-3 text-xs">{caption}</p>
					<div class="border-border bg-surface mt-1 flex items-start gap-2 rounded-md border p-3">
						<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{code}</pre>
						<CopyButton text={() => code} label={caption} />
					</div>
				{/snippet}
				{@render line(publish, 'Publish a version and point production at it')}
				{@render line(fetch, 'Read it back from the Python SDK')}
				<a
					class="text-accent mt-3 inline-block underline underline-offset-2"
					href="https://github.com/tracepad/tracepad/blob/main/docs/prompts.md"
					target="_blank"
					rel="noreferrer"
				>
					Prompts
				</a>
			</div>
		</div>
	{/snippet}
</ListingShell>
