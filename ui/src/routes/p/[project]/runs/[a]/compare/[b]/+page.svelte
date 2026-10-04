<script lang="ts">
	import ArrowLeftRight from '@lucide/svelte/icons/arrow-left-right';
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type ComparedItem, type RunComparison } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import CompareItemDetail from '$lib/components/evals/CompareItemDetail.svelte';
	import Folded from '$lib/components/Folded.svelte';
	import StatusChip from '$lib/components/evals/StatusChip.svelte';
	import VerdictChip from '$lib/components/evals/VerdictChip.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PaginationBar from '$lib/components/PaginationBar.svelte';
	import PeekPanel from '$lib/components/PeekPanel.svelte';
	import { changedOnly, compareHref, deltaText, scoreText, scoreType, short, trim } from '$lib/evals';
	import { Fold } from '$lib/fold.svelte';
	import { ABSENT, cost, count, duration } from '$lib/format';
	import { asPage, Listing, UrlSpot, Walk } from '$lib/listing.svelte';
	import { freshSearch } from '$lib/page';
	import { peekSearch, readPeek } from '$lib/peek';
	import { href } from '$lib/project.svelte';

	// Two runs side by side (spec 016 #10). The page contains no comparison
	// logic: the header block is the response's — means, deltas, how many
	// cases moved which way, the traffic, the metadata that differed, what
	// each run actually ran — and the items are its rows with their verdicts.
	// The one thing decided here is which rows to draw: `?changed=1` hides the
	// ones whose every verdict is `same`, on the page, because the endpoint
	// has no such filter and adding one for a toggle would be the comparison
	// creeping back into a client.

	const POLL_MS = 5000;

	const a = $derived(page.params.a ?? '');
	const b = $derived(page.params.b ?? '');
	const changed = $derived(page.url.searchParams.get('changed') === '1');

	// The header rides on every page of the items, so the listing's read is
	// where it lands (the session panel's shape).
	let compared = $state.raw<RunComparison | null>(null);

	const listing = new Listing<ComparedItem>({
		key: () => `${a}|${b}`,
		spot: new UrlSpot(),
		count: false,
		read: async (at, counting, signal) => {
			const answer = await api.compareRuns(a, b, asPage(at, counting), signal);
			if (!signal.aborted) compared = answer;
			return { ...answer, rows: answer.items };
		},
		failed: 'Failed to compare the runs.'
	});

	// Polling per #7's rule while either run is still open (edge cases): the
	// numbers of a running run move, and so does the page.
	$effect(() => {
		const open = compared?.a.status === 'running' || compared?.b.status === 'running';
		if (!open) return;
		const timer = setInterval(() => {
			if (!document.hidden) listing.tick();
		}, POLL_MS);
		return () => clearInterval(timer);
	});

	const rows = $derived(changed ? changedOnly(listing.rows) : listing.rows);
	const names = $derived(compared ? compared.scores.map((score) => score.name) : []);
	const metadata = $derived(Object.entries(compared?.metadata ?? {}).sort(([x], [y]) => x.localeCompare(y)));

	function toggle() {
		goto(`${page.url.pathname}${freshSearch(changed ? '' : '?changed=1', page.url.searchParams)}`, {
			keepFocus: true
		});
	}

	const peekID = $derived(readPeek(page.url.searchParams).peek);
	const peeked = $derived(listing.rows.find((row) => row.id === peekID) ?? null);
	const walk = new Walk(listing, {
		key: (row) => String(row.seq).padStart(12, '0'),
		peekID: () => peekID,
		showing: () => null,
		open: peek,
		ascending: true,
		// The rows the table draws, not the ones the endpoint sent: with the
		// toggle on, `j`/`k` would otherwise walk into the `same` cases the
		// reader has just asked to hide (found in review of this PR).
		rows: () => rows
	});

	function peek(id: string | null) {
		const search = peekSearch(page.url.searchParams, { peek: id });
		goto(`${page.url.pathname}${search}`, {
			replaceState: id === null || peekID !== null,
			keepFocus: true,
			noScroll: true
		});
	}

	/** One side's aggregate for a name: its mean, or its distribution as words, one per label. */
	function sideOf(score: RunComparison['scores'][number], which: 'a' | 'b'): string[] {
		const value = score[which];
		if (!value) return [ABSENT];
		if (value.distribution) {
			return Object.entries(value.distribution).map(([word, n]) => `${word} ${n}`);
		}
		return [value.mean == null ? ABSENT : trim(value.mean)];
	}

	/** The counts a name reports: improved/regressed for a directed one, changed otherwise. */
	function moved(score: RunComparison['scores'][number]): string[] {
		if (score.improved !== undefined || score.regressed !== undefined) {
			return [`${score.improved ?? 0} improved`, `${score.regressed ?? 0} regressed`, `${score.same} same`];
		}
		return [`${score.changed ?? 0} changed`, `${score.same} same`];
	}

	// In a box narrower than a table its rows fold (spec 006 #24). A score is
	// its name and its delta, with its type, the two sides and how many cases
	// moved under the name; a case is its item and its scores, one per line,
	// with `#seq` and which run attempted it under the item. The numbers are
	// the unfolded tables' widths and their `min-width`: the cases' grows by a
	// column per score name.
	const scoring = new Fold(608);
	const casing = new Fold(() => 312 + 112 * names.length);
	const scoresNarrow = $derived(scoring.narrow);
	const casesNarrow = $derived(casing.narrow);

	const card = 'border-border bg-surface min-w-0 rounded-lg border';
	const head = 'text-subtle border-border border-b px-3 py-2 text-xs font-medium';
	const numeric = 'px-3 py-1.5 text-right tabular-nums';
</script>

<!-- One label of a distribution is cut at 6 rem in a column of its own, and
     at its cell's width where the cell is the box's (spec 006 #24). -->
{#snippet side(score: RunComparison['scores'][number], which: 'a' | 'b', stacked = false)}
	{@const pieces = sideOf(score, which)}
	{#each pieces as piece, i (i)}<span
			class={['inline-block truncate align-bottom', stacked ? 'max-w-full' : 'max-w-24']}
			title={piece}
			>{piece}{i < pieces.length - 1 ? ',' : ''}</span
		>{' '}{/each}
{/snippet}

<!-- A label is what a program named, so each value is cut to its cell rather
     than pushed past it (spec 006 #22, #24). -->
{#snippet outcome(score: ComparedItem['scores'][string] | undefined)}
	{#if score}
		<span class="tabular-nums">
			<span class="inline-block max-w-full truncate align-bottom" title={scoreText(score.a)}>
				{scoreText(score.a)}
			</span>
			→
			<span class="inline-block max-w-full truncate align-bottom" title={scoreText(score.b)}>
				{scoreText(score.b)}
			</span>
		</span>
		<VerdictChip verdict={score.verdict} />
	{:else}
		<span class="text-subtle">{ABSENT}</span>
	{/if}
{/snippet}

<svelte:head><title>Compare · Runs · Tracepad</title></svelte:head>

<PageHeader title="Compare">
	{#snippet meta()}
		<a href={href(`/runs/${a}`)} class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			Run
		</a>
		{#if compared}
			<span class="truncate">{compared.dataset}</span>
			{#if !compared.same_version}
				<span class="text-warn whitespace-nowrap">dataset moved between the runs</span>
			{/if}
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button
			variant={changed ? 'primary' : 'default'}
			aria-pressed={changed}
			onclick={toggle}
			title="Hide the cases whose every verdict is same"
		>
			Changed only
		</Button>
		<!-- Swap: the same comparison the other way round, which the server
		     renders with a and b exchanged — nothing is recomputed here. -->
		<a
			href={href(compareHref(b, a))}
			title="Swap the two runs"
			class="border-border bg-surface text-fg hover:bg-raised pointer-coarse:h-11 pointer-coarse:px-4
				inline-flex h-7 items-center gap-1.5 rounded-md border px-2.5 text-sm font-medium
				whitespace-nowrap transition-colors duration-100"
		>
			<ArrowLeftRight class="size-4" />
			Swap
		</a>
	{/snippet}
</PageHeader>

{#if listing.problem}
	<p role="alert" class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2">
		<TriangleAlert class="size-4 shrink-0" />
		{listing.problem}
	</p>
{/if}

{#if !compared}
	{#if listing.loading}
		<div class="text-subtle flex flex-1 items-center justify-center gap-2">
			<LoaderCircle class="size-4 animate-spin" />
			Comparing the runs
		</div>
	{:else}
		<div class="flex-1"></div>
	{/if}
{:else}
	<div class="flex min-h-0 flex-1 flex-col overflow-auto">
		<div class="border-border grid shrink-0 gap-3 border-b p-4 md:grid-cols-2">
			{#each [['A', compared.a, compared.models.a, compared.prompts.a], ['B', compared.b, compared.models.b, compared.prompts.b]] as const as [letter, run, models, prompts] (letter)}
				<section class={card}>
					<h2 class={head}>{letter}</h2>
					<div class="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm">
						<a href={href(`/runs/${run.id}`)} class="hover:text-accent font-medium">
							{run.name ?? short(run.id)}
						</a>
						<span class="text-subtle tabular-nums">v{run.dataset_version}</span>
						<StatusChip status={run.status} since={run.created_at} />
						<span class="text-subtle font-mono text-xs">{run.id}</span>
					</div>
					<dl class="flex flex-wrap gap-x-6 gap-y-1 px-3 pb-2 text-xs">
						<div class="min-w-0">
							<dt class="text-subtle">Models</dt>
							<dd class="font-mono">{models.length ? models.join(', ') : ABSENT}</dd>
						</div>
						<div class="min-w-0">
							<dt class="text-subtle">Prompts</dt>
							<dd class="font-mono">
								{prompts.length
									? prompts.map((p) => (p.version == null ? p.name : `${p.name}@${p.version}`)).join(', ')
									: ABSENT}
							</dd>
						</div>
					</dl>
				</section>
			{/each}

			<section class={card}>
				<h2 class={head}>Traffic</h2>
				<table class="w-full border-collapse text-sm">
					<thead class="text-subtle text-xs">
						<tr><th scope="col" class="px-3 py-1 text-left font-medium"></th><th scope="col" class="px-3 py-1 text-right font-medium">A</th><th scope="col" class="px-3 py-1 text-right font-medium">B</th></tr>
					</thead>
					<tbody>
						<tr class="border-border border-t"><td class="px-3 py-1">Traces</td><td class={numeric}>{count(compared.traces.count.a)}</td><td class={numeric}>{count(compared.traces.count.b)}</td></tr>
						<tr class="border-border border-t"><td class="px-3 py-1">Failed</td><td class={numeric}>{count(compared.traces.error_count.a)}</td><td class={numeric}>{count(compared.traces.error_count.b)}</td></tr>
						<tr class="border-border border-t">
							<td class="px-3 py-1">Cost</td>
							<td class={numeric}>{cost(compared.traces.total_cost.a)}</td>
							<td class={numeric}>
								{cost(compared.traces.total_cost.b)}
								{#if compared.traces.total_cost.delta != null}
									<span class="text-subtle text-xs">({deltaText(compared.traces.total_cost.delta)})</span>
								{/if}
							</td>
						</tr>
						<tr class="border-border border-t"><td class="px-3 py-1">p50</td><td class={numeric}>{duration(compared.traces.latency_ms.p50.a)}</td><td class={numeric}>{duration(compared.traces.latency_ms.p50.b)}</td></tr>
						<tr class="border-border border-t"><td class="px-3 py-1">p95</td><td class={numeric}>{duration(compared.traces.latency_ms.p95.a)}</td><td class={numeric}>{duration(compared.traces.latency_ms.p95.b)}</td></tr>
					</tbody>
				</table>
			</section>

			<section class={card}>
				<h2 class={head}>Metadata that differs</h2>
				{#if metadata.length === 0}
					<p class="text-subtle px-3 py-4 text-center text-sm">The two runs declared the same things</p>
				{:else}
					<table class="w-full border-collapse text-xs">
						<tbody>
							{#each metadata as [key, pair] (key)}
								<tr class="border-border border-t first:border-t-0">
									<td class="text-subtle max-w-40 truncate px-3 py-1 font-mono" title={key}>{key}</td>
									{#each [pair.a, pair.b] as value, i (i)}
										{@const text = JSON.stringify(value) ?? ABSENT}
										<!-- Wrapped, so it is not cut to a word, and capped, so a document
										     held in a value is not the whole card. -->
										<td class="px-3 py-1 font-mono">
											<div class="line-clamp-6 wrap-anywhere" title={text}>{text}</div>
										</td>
									{/each}
								</tr>
							{/each}
						</tbody>
					</table>
				{/if}
			</section>

			<section class={[card, 'md:col-span-2']}>
				<h2 class={head}>Scores</h2>
				{#if compared.scores.length === 0}
					<p class="text-subtle px-3 py-4 text-center text-sm">Neither run carries a score</p>
				{:else}
					<div bind:contentRect={scoring.rect} class="overflow-x-auto">
						<table class="w-full border-collapse text-left text-sm" style:min-width={scoring.min}>
							<thead class="text-subtle text-xs whitespace-nowrap">
								<tr class="border-border border-b">
									<th scope="col" class="px-3 py-1.5 font-medium">Name</th>
									{#if !scoresNarrow}
										<th scope="col" class="w-32 px-3 py-1.5 font-medium">Type</th>
										<th scope="col" class="w-44 px-3 py-1.5 text-right font-medium">A</th>
										<th scope="col" class="w-44 px-3 py-1.5 text-right font-medium">B</th>
									{/if}
									<th scope="col" class="w-20 px-3 py-1.5 text-right font-medium">Delta</th>
									{#if !scoresNarrow}
										<th scope="col" class="w-64 px-3 py-1.5 font-medium">Moved</th>
									{/if}
								</tr>
							</thead>
							<tbody>
								{#each compared.scores as score (score.name)}
									{@const type = scoreType(score)}
									<tr class={['border-border border-b last:border-b-0', scoresNarrow && 'align-top']}>
										{#if scoresNarrow}
											<td class="max-w-0 px-3 py-1.5">
												<div class="truncate font-medium" title={score.name}>{score.name}</div>
												<div class="tabular-nums">{@render side(score, 'a', true)} → {@render side(score, 'b', true)}</div>
												<div class="text-muted text-xs tabular-nums">
													<Folded values={[type, ...moved(score)]} />
												</div>
											</td>
										{:else}
											<td class="max-w-0 min-w-32 truncate px-3 py-1.5 font-medium" title={score.name}>
												{score.name}
											</td>
											<td class="text-muted px-3 py-1.5">{type}</td>
											<td class={[numeric, 'min-w-26']}>{@render side(score, 'a')}</td>
											<td class={[numeric, 'min-w-26']}>{@render side(score, 'b')}</td>
										{/if}
										<td class={[numeric, (score.delta ?? 0) > 0 && score.direction === 'higher' && 'text-ok', (score.delta ?? 0) < 0 && score.direction === 'higher' && 'text-danger']}>
											{deltaText(score.delta)}
										</td>
										{#if !scoresNarrow}
											<td class="text-muted px-3 py-1.5 text-xs tabular-nums">{moved(score).join(' · ')}</td>
										{/if}
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{/if}
			</section>
		</div>

		{#if listing.rows.length > 0 || !listing.newest}
			<div bind:contentRect={casing.rect} class="min-h-0 shrink-0 overflow-x-auto">
				<table
					aria-label="Cases"
					class="w-full table-fixed border-collapse text-left"
					style:min-width={casing.min}
				>
					<thead class="bg-canvas text-subtle text-xs whitespace-nowrap">
						<tr class="border-border border-b">
							{#if casesNarrow}
								<th scope="col" class="w-28 px-3 py-2 font-medium">Item</th>
								{#if names.length > 0}<th scope="col" class="px-3 py-2 font-medium">Scores</th>{/if}
							{:else}
								<th scope="col" class="w-14 px-3 py-2 text-right font-medium">#</th>
								<th scope="col" class="w-28 px-3 py-2 font-medium">Item</th>
								<th scope="col" class="w-36 px-3 py-2 font-medium">In</th>
								{#each names as name (name)}
									<th scope="col" class="truncate px-3 py-2 font-medium" title={name}>{name}</th>
								{/each}
							{/if}
						</tr>
					</thead>
					<tbody>
						{#each rows as row (row.id)}
							{@const lit = row.id === peekID}
							<tr
								class={[
									'border-border hover:bg-raised border-b transition-colors duration-100',
									lit && 'bg-accent-soft',
									casesNarrow && 'align-top'
								]}
							>
								{#if !casesNarrow}
									<td class="text-muted px-3 py-1.5 text-right tabular-nums">{row.seq}</td>
								{/if}
								<td class="truncate px-3 py-1.5 font-mono text-xs">
									<a
										href={peekSearch(page.url.searchParams, { peek: row.id })}
										aria-current={lit ? 'true' : undefined}
										title={row.id}
										onclick={(event) => {
											if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
											event.preventDefault();
											peek(row.id);
										}}
									>
										{short(row.id)}
									</a>
									{#if casesNarrow}
										<!-- The cell is `truncate`, which is `nowrap` too. -->
										<div class="text-muted font-sans whitespace-normal tabular-nums">
											<Folded values={[`#${row.seq}`, row.in.replaceAll('_', ' ')]} />
										</div>
									{/if}
								</td>
								{#if casesNarrow}
									{#if names.length > 0}
										<td class="px-3 py-1.5 text-xs">
											<ul class="space-y-0.5">
												{#each names as name (name)}
													<li class="flex flex-wrap items-center gap-x-2">
														<span class="text-subtle max-w-full min-w-0 truncate" title={name}>{name}</span>
														{@render outcome(row.scores[name])}
													</li>
												{/each}
											</ul>
										</td>
									{/if}
								{:else}
									<td class="text-muted px-3 py-1.5 text-xs">{row.in.replaceAll('_', ' ')}</td>
									{#each names as name (name)}
										<td class="px-3 py-1.5 text-xs">{@render outcome(row.scores[name])}</td>
									{/each}
								{/if}
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			{#if rows.length === 0 && changed && listing.rows.length > 0}
				<p class="text-subtle px-4 py-6 text-center">Every case on this page came out the same.</p>
			{/if}
			<PaginationBar {...listing.bar} noun="item" />
		{:else if !listing.loading && !listing.failure}
			<p class="text-subtle p-8 text-center">{ABSENT} Neither run attempted a case.</p>
		{/if}
	</div>
{/if}

{#if peekID && compared}
	<PeekPanel
		label="Case"
		onclose={() => peek(null)}
		onprev={() => walk.step(-1)}
		onnext={() => walk.step(1)}
		hasPrev={walk.hasPrev}
		hasNext={walk.hasNext}
		fullHref={href(
			`/datasets/${encodeURIComponent(compared.dataset)}?version=${compared.a.dataset_version}&peek=${peekID}`
		)}
		fullLabel="Open this item in its dataset"
	>
		{#snippet title()}
			<h2 class="shrink-0 text-lg font-semibold tracking-tight">Item {short(peekID)}</h2>
		{/snippet}
		{#snippet meta()}
			{#if peeked}
				<span class="text-xs">{peeked.in.replaceAll('_', ' ')}</span>
			{/if}
			<span class="hidden truncate font-mono md:inline">{peekID}</span>
		{/snippet}
		<CompareItemDetail item={peeked} {compared} />
	</PeekPanel>
{/if}
