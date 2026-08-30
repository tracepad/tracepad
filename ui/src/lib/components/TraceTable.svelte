<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import type { TraceRow } from '$lib/api/client.svelte';
	import { ABSENT, cost, duration, timestamp, wait } from '$lib/format';
	import { modified, selecting } from '$lib/peek';
	import { highlight, searchTerms } from '$lib/search';

	// The listing, one row per trace, mapping 1:1 onto what
	// `GET /api/v1/traces` returns (Application contract). Nothing is computed
	// here that the API did not send.

	let {
		rows,
		onopen,
		selectedID = null,
		search = ''
	}: {
		rows: TraceRow[];
		/**
		 * What an unmodified left click does instead of following the link —
		 * every caller opens the row in a peek panel (spec 008 #3). Left out,
		 * the row is a plain link to the full page. A row that matched a search
		 * hands over the observation it matched, so the panel opens on it
		 * rather than on the top of the trace (spec 011, Application contract).
		 */
		onopen?: (id: string, observationID?: string | null) => void;
		/** The row the panel is showing, lit so that it is clear where it came from. */
		selectedID?: string | null;
		/** The search these rows answer, for marking its terms in the snippets. */
		search?: string;
	} = $props();

	const terms = $derived(searchTerms(search));

	/**
	 * The whole row opens the panel, but the row's cells stay ordinary
	 * selectable text (spec 008 #15): a click that came out of a selection
	 * opens nothing, and it does not follow the row's link either.
	 */
	function open(event: MouseEvent, row: TraceRow) {
		if (!onopen || modified(event)) return;
		event.preventDefault();
		if (selecting(event)) return;
		// The panel gives focus back to whatever opened it, and a click on a
		// cell leaves it on the body; the row's own link is where the reader
		// actually is (PR #10 review).
		(event.currentTarget as HTMLElement).closest('tbody')?.querySelector('a')?.focus();
		onopen(row.id, row.match?.observation_id ?? null);
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
				<!-- Beside latency, because they answer the same question from two
				     ends: how long the whole thing took, and how long the person
				     waited before anything appeared (spec 012, Application
				     contract). -->
				<th scope="col" class="w-24 px-3 py-2 text-right font-medium" title="Time to first token">
					TTFT
				</th>
				<th scope="col" class="w-24 px-3 py-2 font-medium">Errors</th>
			</tr>
		</thead>
		<!-- One `tbody` per trace, because a row that matched a search is two
		     lines and they are one row: the hover, the click and the lit
		     selection belong to both (spec 011, Application contract). -->
		{#each rows as row (row.id)}
			{@const lit = row.id === selectedID}
			<tbody class="group">
				<!-- The click is on the row and the link is inside it, rather than a
				     link stretched over the row: an overlay that covers the cells
				     makes every value in them undraggable, and a listing whose ids
				     cannot be copied out is a listing you have to retype from
				     (spec 008 #15). The keyboard path is the link. -->
				<!-- svelte-ignore a11y_click_events_have_key_events -->
				<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
				<tr
					onclick={(event) => open(event, row)}
					class={[
						'border-border group-hover:bg-raised transition-colors duration-100',
						!row.match && 'border-b',
						onopen && 'cursor-pointer',
						lit && 'bg-accent-soft'
					]}
				>
					<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
						<!-- Exactly one thing in the row is tabbable, and it is a real
						     link: ⌘-click, a middle click and anything that reads links
						     get the page it points at. Enter opens the panel, the same
						     as a plain click does (spec 008 #16). -->
						<a
							href="/traces/{row.id}{row.match?.observation_id
								? `?obs=${encodeURIComponent(row.match.observation_id)}`
								: ''}"
							aria-current={lit ? 'true' : undefined}
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
					<td class="text-muted {numeric}">{wait(row.ttft_ms)}</td>
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
				{#if row.match}
					<!-- svelte-ignore a11y_click_events_have_key_events -->
					<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
					<tr
						onclick={(event) => open(event, row)}
						class={[
							'border-border group-hover:bg-raised border-b transition-colors duration-100',
							onopen && 'cursor-pointer',
							lit && 'bg-accent-soft'
						]}
					>
						<td class="text-muted px-3 pt-0 pb-1.5 font-mono text-xs" colspan="9">
							<span class="text-subtle">{row.match.field}</span>
							{#each highlight(row.match.snippet, terms) as piece, i (i)}
								{#if piece.hit}<mark class="bg-accent-soft text-fg rounded-sm px-0.5"
										>{piece.text}</mark
									>{:else}{piece.text}{/if}
							{/each}
						</td>
					</tr>
				{/if}
			</tbody>
		{/each}
	</table>
</div>
