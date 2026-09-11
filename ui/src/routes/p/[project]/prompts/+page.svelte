<script lang="ts">
	import ScrollText from '@lucide/svelte/icons/scroll-text';
	import { goto } from '$app/navigation';
	import { api, type PromptRow } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import CopyButton from '$lib/components/CopyButton.svelte';
	import ListingCount from '$lib/components/ListingCount.svelte';
	import ListingShell from '$lib/components/ListingShell.svelte';
	import LabelChip from '$lib/components/prompts/LabelChip.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
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

	const cell = 'truncate px-3 py-1.5';
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
		<div class="min-h-0 flex-1 overflow-auto">
			<table class="w-full min-w-2xl border-collapse text-left">
				<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
					<tr class="border-border border-b">
						<th scope="col" class="w-64 px-3 py-2 font-medium">Name</th>
						<th scope="col" class="w-20 px-3 py-2 font-medium">Type</th>
						<th scope="col" class="w-20 px-3 py-2 text-right font-medium">Latest</th>
						<th scope="col" class="px-3 py-2 font-medium">Labels</th>
						<th scope="col" class="w-44 px-3 py-2 font-medium">Updated</th>
					</tr>
				</thead>
				<tbody>
					{#each listing.rows as row (row.name)}
						<tr class="border-border hover:bg-raised border-b transition-colors duration-100">
							<td class="{cell} font-medium">
								<a href={href(`/prompts/${encodeURIComponent(row.name)}`)} class="hover:text-accent">
									{row.name}
								</a>
							</td>
							<td class="text-muted {cell}">{row.type}</td>
							<td class="text-muted px-3 py-1.5 text-right tabular-nums">v{row.latest_version}</td>
							<td class="px-3 py-1.5">
								<div class="flex flex-wrap gap-1">
									{#each orderLabelEntries(row.labels) as [label, version] (label)}
										<LabelChip {label} {version} />
									{/each}
								</div>
							</td>
							<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
								{timestamp(row.updated_at)}
							</td>
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
