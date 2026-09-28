<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import type { SessionRow } from '$lib/api/client.svelte';
	import { Fold } from '$lib/fold.svelte';
	import { ABSENT, cost, count, counted, timestamp } from '$lib/format';
	import { modified, selecting } from '$lib/peek';
	import { href } from '$lib/project.svelte';
	import Folded from './Folded.svelte';

	// The session listing, one row per session, mapping 1:1 onto what
	// `GET /api/v1/sessions` returns. Every number counts traces, which is what
	// a session is a collection of — the column headers say so rather than
	// leaving it to be guessed.

	let {
		rows,
		onopen,
		selectedID = null
	}: {
		rows: SessionRow[];
		/** An unmodified left click opens the peek panel instead (spec 008 #3). */
		onopen?: (id: string) => void;
		/** The row the panel is showing. */
		selectedID?: string | null;
	} = $props();

	/** The row opens the panel; its cells stay selectable text (spec 008 #15). */
	function open(event: MouseEvent, id: string) {
		if (!onopen || modified(event)) return;
		event.preventDefault();
		if (selecting(event)) return;
		// So that closing the panel returns focus to the row (PR #10 review).
		(event.currentTarget as HTMLElement).querySelector('a')?.focus();
		onopen(id);
	}

	const numeric = 'px-3 py-1.5 text-right tabular-nums';

	// In a box narrower than the table the row is when it was last seen, which
	// session and whether any of it failed; how many traces, what they cost
	// and when it began fold under the id (spec 006 #22). The number is the
	// unfolded table's width and its `min-width`.
	const fold = new Fold(720);
	const narrow = $derived(fold.narrow);
</script>

<!-- The table scrolls inside its own box; the page never scrolls sideways
     (spec 006 #15), and folds in a box narrower than itself (#22). -->
<div bind:contentRect={fold.rect} class="min-h-0 flex-1 overflow-auto">
	<table class="w-full border-collapse text-left" style:min-width={fold.min}>
		<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
			<tr class="border-border border-b">
				<th scope="col" class={['px-3 py-2 font-medium', !narrow && 'w-44']}>Last seen</th>
				<th scope="col" class={['px-3 py-2 font-medium', narrow && 'w-full']}>Session</th>
				{#if !narrow}
					<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Traces</th>
				{/if}
				<th scope="col" class={['px-3 py-2 font-medium', !narrow && 'w-28']}>Errors</th>
				{#if !narrow}
					<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Cost</th>
					<th scope="col" class="w-44 px-3 py-2 font-medium">First seen</th>
				{/if}
			</tr>
		</thead>
		<tbody>
			{#each rows as row (row.id)}
				<!-- The click is on the row, not on a link stretched over it: an
				     overlay across the cells would make the session id in them
				     impossible to select and copy (spec 008 #15). -->
				<!-- svelte-ignore a11y_click_events_have_key_events -->
				<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
				<tr
					onclick={(event) => open(event, row.id)}
					class={[
						'border-border hover:bg-raised border-b transition-colors duration-100',
						onopen && 'cursor-pointer',
						row.id === selectedID && 'bg-accent-soft'
					]}
				>
					<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
						<!-- Exactly one thing in the row is tabbable, and it is a real
						     link: ⌘-click and anything that reads links get the page it
						     points at. Enter opens the panel, as a plain click does
						     (spec 008 #16). -->
						<a
							href={href(`/sessions/${encodeURIComponent(row.id)}`)}
							aria-current={row.id === selectedID ? 'true' : undefined}
							title={row.id}
						>
							{timestamp(row.last_seen)}
						</a>
					</td>
					{#if narrow}
						<td class="max-w-0 px-3 py-1.5">
							<div class="truncate font-mono" title={row.id}>{row.id}</div>
							<div class="text-muted text-xs tabular-nums">
								<Folded values={[counted(row.trace_count, 'trace'), cost(row.total_cost)]} />
							</div>
							<!-- Not a `Folded` line: this column is the narrowest on a phone, and
							     a value of its own is cut where a line of text is not. -->
							<div class="text-muted text-xs tabular-nums">first seen {timestamp(row.first_seen)}</div>
						</td>
					{:else}
						<td class="max-w-0 min-w-48 truncate px-3 py-1.5 font-mono" title={row.id}>{row.id}</td>
						<td class="text-muted {numeric}">{count(row.trace_count)}</td>
					{/if}
					<td class="px-3 py-1.5 whitespace-nowrap">
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
					{#if !narrow}
						<td class="text-muted {numeric}">{cost(row.total_cost)}</td>
						<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
							{timestamp(row.first_seen)}
						</td>
					{/if}
				</tr>
			{/each}
		</tbody>
	</table>
</div>
