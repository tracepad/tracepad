<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import type { SessionRow } from '$lib/api/client.svelte';
	import { ABSENT, cost, count, timestamp } from '$lib/format';

	// The session listing, one row per session, mapping 1:1 onto what
	// `GET /api/v1/sessions` returns. Every number counts traces, which is what
	// a session is a collection of — the column headers say so rather than
	// leaving it to be guessed.

	let { rows }: { rows: SessionRow[] } = $props();

	const numeric = 'px-3 py-1.5 text-right tabular-nums';
</script>

<!-- The table scrolls inside its own box; the page never scrolls sideways
     (spec 006 #15). -->
<div class="min-h-0 flex-1 overflow-auto">
	<table class="w-full min-w-2xl border-collapse text-left">
		<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
			<tr class="border-border border-b">
				<th scope="col" class="w-44 px-3 py-2 font-medium">Last seen</th>
				<th scope="col" class="px-3 py-2 font-medium">Session</th>
				<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Traces</th>
				<th scope="col" class="w-28 px-3 py-2 font-medium">Errors</th>
				<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Cost</th>
				<th scope="col" class="w-44 px-3 py-2 font-medium">First seen</th>
			</tr>
		</thead>
		<tbody>
			{#each rows as row (row.id)}
				<tr class="border-border hover:bg-raised relative border-b transition-colors duration-100">
					<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
						<!-- The link covers the row, so the whole row is clickable and
						     exactly one thing is tabbable. -->
						<a
							href="/sessions/{encodeURIComponent(row.id)}"
							class="after:absolute after:inset-0 after:content-['']"
							title={row.id}
						>
							{timestamp(row.last_seen)}
						</a>
					</td>
					<td class="truncate px-3 py-1.5 font-mono">{row.id}</td>
					<td class="text-muted {numeric}">{count(row.trace_count)}</td>
					<td class="px-3 py-1.5">
						{#if row.error_count > 0}
							<!-- Colour is never the message on its own. -->
							<span
								class="text-danger bg-danger-soft inline-flex items-center gap-1 rounded px-1.5
									py-0.5 text-xs font-medium tabular-nums"
							>
								<TriangleAlert class="size-3.5" />
								{row.error_count}
								{row.error_count === 1 ? 'trace' : 'traces'}
							</span>
						{:else}
							<span class="text-subtle text-xs">{ABSENT}</span>
						{/if}
					</td>
					<td class="text-muted {numeric}">{cost(row.total_cost)}</td>
					<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
						{timestamp(row.first_seen)}
					</td>
				</tr>
			{/each}
		</tbody>
	</table>
</div>
