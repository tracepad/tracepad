<script lang="ts">
	import type { RunWithSummary } from '$lib/api/client.svelte';
	import { trim } from '$lib/evals';
	import { ABSENT, cost, count, duration } from '$lib/format';

	// A run's summary as the server computed it (spec 014 API contract →
	// Runs): coverage, traffic, the scores table, what actually ran, and what
	// the harness said it was trying. Rendered, not computed — every number is
	// the response's (spec 004 #1).

	let {
		summary,
		metadata
	}: { summary: RunWithSummary['summary']; metadata: RunWithSummary['metadata'] } = $props();

	const names = $derived(Object.keys(summary.scores).sort());
	const entries = $derived(Object.entries(metadata ?? {}).sort(([a], [b]) => a.localeCompare(b)));

	/** A categorical name's counts, largest first, for the distribution bar. */
	function distribution(stat: RunWithSummary['summary']['scores'][string]) {
		const counts = Object.entries(stat.distribution ?? {}).sort(([, a], [, b]) => b - a);
		const most = counts[0]?.[1] ?? 1;
		return counts.map(([value, n]) => ({ value, n, share: (100 * n) / most }));
	}

	const card = 'border-border bg-surface min-w-0 rounded-lg border';
	const head = 'text-subtle border-border border-b px-3 py-2 text-xs font-medium';
	const figure = 'flex flex-wrap gap-x-6 gap-y-2 px-3 py-2 text-sm';
</script>

<div class="border-border grid shrink-0 gap-3 border-b p-4 md:grid-cols-2 xl:grid-cols-3">
	<section class={card}>
		<h2 class={head}>Coverage</h2>
		<dl class={figure}>
			{#each [['Items', summary.items.total], ['Covered', summary.items.covered], ['Missing', summary.items.missing], ['Unknown traces', summary.items.unknown]] as [label, value] (label)}
				<div>
					<dt class="text-subtle text-xs">{label}</dt>
					<dd class="tabular-nums">{count(Number(value))}</dd>
				</div>
			{/each}
		</dl>
	</section>

	<section class={card}>
		<h2 class={head}>Traffic</h2>
		<dl class={figure}>
			<div><dt class="text-subtle text-xs">Traces</dt><dd class="tabular-nums">{count(summary.traces.count)}</dd></div>
			<div><dt class="text-subtle text-xs">Attempts max</dt><dd class="tabular-nums">{count(summary.traces.attempts_max)}</dd></div>
			<div>
				<dt class="text-subtle text-xs">Failed</dt>
				<dd class={['tabular-nums', summary.traces.error_count > 0 && 'text-danger']}>
					{count(summary.traces.error_count)}
				</dd>
			</div>
			<div><dt class="text-subtle text-xs">Cost</dt><dd class="tabular-nums">{cost(summary.traces.total_cost)}</dd></div>
			<div><dt class="text-subtle text-xs">p50</dt><dd class="tabular-nums">{duration(summary.traces.latency_ms.p50)}</dd></div>
			<div><dt class="text-subtle text-xs">p95</dt><dd class="tabular-nums">{duration(summary.traces.latency_ms.p95)}</dd></div>
		</dl>
	</section>

	<section class={card}>
		<h2 class={head}>Ran</h2>
		<dl class={figure}>
			<div class="min-w-0">
				<dt class="text-subtle text-xs">Models</dt>
				<dd class="font-mono text-xs">{summary.models.length ? summary.models.join(', ') : ABSENT}</dd>
			</div>
			<div class="min-w-0">
				<dt class="text-subtle text-xs">Prompts</dt>
				<dd class="font-mono text-xs">
					{#if summary.prompts.length === 0}
						{ABSENT}
					{:else}
						{#each summary.prompts as prompt, i (`${prompt.name}@${prompt.version}`)}
							{i > 0 ? ', ' : ''}<a
								class="hover:text-accent underline underline-offset-2"
								href="/traces?prompt={encodeURIComponent(prompt.version == null ? prompt.name : `${prompt.name}@${prompt.version}`)}"
							>
								{prompt.name}{prompt.version == null ? '' : `@${prompt.version}`}
							</a>
						{/each}
					{/if}
				</dd>
			</div>
		</dl>
	</section>

	<section class={[card, 'md:col-span-2']}>
		<h2 class={head}>Scores</h2>
		{#if names.length === 0}
			<p class="text-subtle px-3 py-4 text-center text-sm">No score has been posted against this run's traces</p>
		{:else}
			<div class="overflow-x-auto">
				<table class="w-full min-w-lg border-collapse text-left text-sm">
					<thead class="text-subtle text-xs whitespace-nowrap">
						<tr class="border-border border-b">
							<th scope="col" class="px-3 py-1.5 font-medium">Name</th>
							<th scope="col" class="w-28 px-3 py-1.5 font-medium">Type</th>
							<th scope="col" class="w-20 px-3 py-1.5 text-right font-medium">Count</th>
							<th scope="col" class="w-20 px-3 py-1.5 text-right font-medium">Mean</th>
							<th scope="col" class="w-64 px-3 py-1.5 font-medium">Range or distribution</th>
						</tr>
					</thead>
					<tbody>
						{#each names as name (name)}
							{@const stat = summary.scores[name]}
							<tr class="border-border border-b last:border-b-0">
								<td class="px-3 py-1.5 font-medium">{name}</td>
								<td class="text-muted px-3 py-1.5">
									{stat.data_type}{stat.direction ? ` · ${stat.direction}` : ''}
								</td>
								<td class="text-muted px-3 py-1.5 text-right tabular-nums">{count(stat.count)}</td>
								<td class="px-3 py-1.5 text-right tabular-nums">
									{stat.mean == null ? ABSENT : trim(stat.mean)}
								</td>
								<td class="text-muted px-3 py-1.5 tabular-nums">
									{#if stat.distribution}
										<ul class="space-y-0.5">
											{#each distribution(stat) as part (part.value)}
												<li class="flex items-center gap-2 text-xs">
													<span class="w-20 truncate" title={part.value}>{part.value}</span>
													<span class="bg-raised h-2 flex-1 overflow-hidden rounded-sm">
														<span class="bg-accent block h-full" style:width="{part.share}%"></span>
													</span>
													<span class="w-8 text-right">{count(part.n)}</span>
												</li>
											{/each}
										</ul>
									{:else if stat.min != null && stat.max != null}
										{trim(stat.min)} … {trim(stat.max)}
									{:else}
										{ABSENT}
									{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>

	<section class={card}>
		<h2 class={head}>Metadata</h2>
		{#if entries.length === 0}
			<p class="text-subtle px-3 py-4 text-center text-sm">The harness declared nothing</p>
		{:else}
			<dl class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 px-3 py-2 text-sm">
				{#each entries as [key, value] (key)}
					<dt class="text-subtle truncate font-mono text-xs" title={key}>{key}</dt>
					<dd class="truncate font-mono text-xs" title={JSON.stringify(value)}>{JSON.stringify(value)}</dd>
				{/each}
			</dl>
		{/if}
	</section>
</div>
