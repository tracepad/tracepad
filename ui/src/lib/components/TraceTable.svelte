<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import type { TraceRow } from '$lib/api/client.svelte';
	import { ABSENT, cost, duration, timestamp } from '$lib/format';
	import { modified } from '$lib/peek';

	// The listing, one row per trace, mapping 1:1 onto what
	// `GET /api/v1/traces` returns (Application contract). Nothing is computed
	// here that the API did not send.

	let {
		rows,
		onopen,
		selectedID = null
	}: {
		rows: TraceRow[];
		/**
		 * What an unmodified left click does instead of following the link —
		 * every caller opens the row in a peek panel (spec 008 #3). Left out,
		 * the row is a plain link to the full page.
		 */
		onopen?: (id: string) => void;
		/** The row the panel is showing, lit so that it is clear where it came from. */
		selectedID?: string | null;
	} = $props();

	function open(event: MouseEvent, id: string) {
		if (!onopen || modified(event)) return;
		event.preventDefault();
		onopen(id);
	}

	// A trace with a failing observation says so in words as well as in colour
	// (accessibility floor): colour alone is not a message.
	const cell = 'truncate px-3 py-1.5';
	const numeric = 'px-3 py-1.5 text-right tabular-nums';
</script>

<!-- The table scrolls inside its own box; the page never scrolls sideways
     (spec 006 #15). -->
<div class="min-h-0 flex-1 overflow-auto">
	<table class="w-full min-w-3xl border-collapse text-left">
		<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
			<tr class="border-border border-b">
				<th scope="col" class="w-44 px-3 py-2 font-medium">Time</th>
				<th scope="col" class="px-3 py-2 font-medium">Name</th>
				<th scope="col" class="w-28 px-3 py-2 font-medium">Environment</th>
				<th scope="col" class="w-36 px-3 py-2 font-medium">User</th>
				<th scope="col" class="w-36 px-3 py-2 font-medium">Session</th>
				<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Cost</th>
				<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Latency</th>
				<th scope="col" class="w-24 px-3 py-2 font-medium">Errors</th>
			</tr>
		</thead>
		<tbody>
			{#each rows as row (row.id)}
				<tr
					class={[
						'border-border hover:bg-raised relative border-b transition-colors duration-100',
						row.id === selectedID && 'bg-accent-soft'
					]}
				>
					<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
						<!-- The link covers the row, so the whole row is clickable and
						     exactly one thing is tabbable. A plain click opens the peek
						     panel; ⌘-click still opens the page it points at. -->
						<a
							href="/traces/{row.id}"
							onclick={(event) => open(event, row.id)}
							aria-current={row.id === selectedID ? 'true' : undefined}
							class="after:absolute after:inset-0 after:content-['']"
							title={row.id}
						>
							{timestamp(row.timestamp)}
						</a>
					</td>
					<td class={cell}>{row.name ?? ABSENT}</td>
					<td class="text-muted {cell}">{row.environment}</td>
					<td class="text-muted {cell}">{row.user_id ?? ABSENT}</td>
					<td class="text-muted {cell}">{row.session_id ?? ABSENT}</td>
					<td class="text-muted {numeric}">{cost(row.total_cost)}</td>
					<td class="text-muted {numeric}">{duration(row.latency_ms)}</td>
					<td class="px-3 py-1.5">
						{#if row.error_count > 0}
							<span
								class="text-danger bg-danger-soft inline-flex items-center gap-1 rounded px-1.5
									py-0.5 text-xs font-medium tabular-nums"
							>
								<TriangleAlert class="size-3.5" />
								{row.error_count}
								{row.error_count === 1 ? 'error' : 'errors'}
							</span>
						{:else}
							<span class="text-subtle text-xs">{ABSENT}</span>
						{/if}
					</td>
				</tr>
			{/each}
		</tbody>
	</table>
</div>
