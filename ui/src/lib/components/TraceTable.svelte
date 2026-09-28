<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import type { TraceRow } from '$lib/api/client.svelte';
	import { ABSENT, cost, duration, timestamp, wait } from '$lib/format';
	import { modified, selecting } from '$lib/peek';
	import { Fold } from '$lib/fold.svelte';
	import { href } from '$lib/project.svelte';
	import { highlight, searchTerms } from '$lib/search';
	import Folded from './Folded.svelte';

	// The listing, one row per trace, mapping 1:1 onto what
	// `GET /api/v1/traces` returns (Application contract). Nothing is computed
	// here that the API did not send.

	let {
		rows,
		onopen,
		selectedID = null,
		search = '',
		linkSession = true
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
		/**
		 * Whether the session id is a link (spec 023 #16). It is not on a table
		 * that is already inside the session: a link to where the reader stands
		 * is not a destination, and from a peek panel it tears down the listing
		 * to arrive at it (spec 023 #17, from review).
		 */
		linkSession?: boolean;
	} = $props();

	const terms = $derived(searchTerms(search));

	// In a box narrower than the table the row is when, what and whether it
	// failed, and the other columns fold under the name: where it ran, how
	// long it took and what it cost on one line, whose and which session on
	// the next (spec 006 #18, #22). The number is the unfolded table's width and
	// its `min-width`: nine columns at the widths their usual values take.
	const fold = new Fold(896);
	const narrow = $derived(fold.narrow);
	const firstToken = (row: TraceRow) =>
		row.ttft_ms == null ? null : `TTFT ${wait(row.ttft_ms)}`;

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
	// Each id on a folded line is cut on its own, so a long user id cannot push
	// the session past the edge, and on a finger it is a 24 px target rather
	// than a line of small text (spec 006 #15, #18).
	const foldedLink = 'min-w-0 max-w-full truncate pointer-coarse:py-1';
</script>

<!-- The user id is a link to their page (spec 023, Application contract). It
     is the second tabbable thing in the row, and deliberately so: "everything
     this account did" is a destination, not a decoration. The row's own click
     still opens the panel, which is why the link stops the event. -->
{#snippet user(row: TraceRow, fold = false)}
	{#if row.user_id}
		<a
			href={href(`/users/${encodeURIComponent(row.user_id)}`)}
			onclick={(event) => event.stopPropagation()}
			title="Everything about {row.user_id}"
			class={['hover:text-fg hover:underline', fold && foldedLink]}
		>
			{row.user_id}
		</a>
	{:else}
		{ABSENT}
	{/if}
{/snippet}

<!-- And the session id is a link to the session (spec 023 #16), built the same
     way for the same reason: a session is the reading unit, and the trace table
     is where a reader meets one. The third tab stop in the row — except where
     the reader is already in that session, and the cell stays the text it was
     (#17). Both stay on a folded row: the panel's own meta leaves the session
     out below `md`, and a destination a phone cannot reach is not one there. -->
{#snippet session(row: TraceRow, fold = false)}
	{#if !row.session_id}
		{ABSENT}
	{:else if linkSession}
		<a
			href={href(`/sessions/${encodeURIComponent(row.session_id)}`)}
			onclick={(event) => event.stopPropagation()}
			title="Everything in {row.session_id}"
			class={['hover:text-fg hover:underline', fold && foldedLink]}
		>
			{row.session_id}
		</a>
	{:else}
		<span class={[fold && foldedLink]}>{row.session_id}</span>
	{/if}
{/snippet}

<!-- The table scrolls inside its own box; the page never scrolls sideways
     (spec 006 #15). In a box narrower than itself it has three columns and
     nothing to scroll to (#18, #22). -->
<div bind:clientWidth={fold.box} class="min-h-0 flex-1 overflow-auto">
	<table
		class="w-full border-collapse text-left"
		style:min-width={fold.min}
	>
		<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
			<tr class="border-border border-b">
				<th scope="col" class={['px-3 py-2 font-medium', !narrow && 'w-44']}>Time</th>
				<!-- `w-full` beside the cell's `max-w-0`: the name takes what the
				     other two leave and is cut to an ellipsis there, rather than
				     widening the table past the screen. -->
				<th scope="col" class={['px-3 py-2 font-medium', narrow && 'w-full']}>Name</th>
				{#if !narrow}
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
				{/if}
				<th scope="col" class={['px-3 py-2 font-medium', !narrow && 'w-24']}>Errors</th>
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
						<!-- The row's own link, and the first in it: ⌘-click, a middle
						     click and anything that reads links get the page it points
						     at. Enter opens the panel, the same as a plain click does
						     (spec 008 #16).

						     It was the *only* tabbable thing in the row until spec 023
						     made the user and the session ids links too. Three stops
						     rather than one, on purpose: "everything this account did"
						     and "everything in this session" are destinations, and a
						     destination reachable only with a mouse is not one. -->
						<a
							href={href(
								`/traces/${row.id}${
									row.match?.observation_id
										? `?obs=${encodeURIComponent(row.match.observation_id)}`
										: ''
								}`
							)}
							aria-current={lit ? 'true' : undefined}
							title={row.id}
						>
							{timestamp(row.timestamp)}
						</a>
					</td>
					{#if narrow}
						<td class="max-w-0 px-3 py-1.5">
							<div class="truncate">{row.name ?? ABSENT}</div>
							<div class="text-muted text-xs tabular-nums">
								<Folded
									values={[
										row.environment,
										duration(row.latency_ms),
										firstToken(row),
										cost(row.total_cost)
									]}
								/>
							</div>
							{#if row.user_id || row.session_id}
								<div class="text-muted flex flex-wrap items-center gap-x-1 text-xs">
									{#if row.user_id}{@render user(row, true)}{/if}
									{#if row.user_id && row.session_id}<span aria-hidden="true">·</span>{/if}
									{#if row.session_id}{@render session(row, true)}{/if}
								</div>
							{/if}
						</td>
					{:else}
						<td class={cell}>{row.name ?? ABSENT}</td>
						<td class="text-muted {cell}">{row.environment}</td>
						<td class="text-muted {cell}">{@render user(row)}</td>
						<td class="text-muted {cell}">{@render session(row)}</td>
						<td class="text-muted {numeric}">{cost(row.total_cost)}</td>
						<td class="text-muted {numeric}">{duration(row.latency_ms)}</td>
						<td class="text-muted {numeric}">{wait(row.ttft_ms)}</td>
					{/if}
					<td class="px-3 py-1.5 whitespace-nowrap">
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
						<td class="text-muted px-3 pt-0 pb-1.5 font-mono text-xs" colspan={narrow ? 3 : 9}>
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
