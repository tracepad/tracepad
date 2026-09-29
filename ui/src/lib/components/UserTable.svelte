<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import type { UserRow } from '$lib/api/client.svelte';
	import { Fold } from '$lib/fold.svelte';
	import { ABSENT, cost, count, counted, middleEllipsis, timestamp } from '$lib/format';
	import { href } from '$lib/project.svelte';
	import { billedTokens, compact, tokenClasses } from '$lib/tokens';
	import CopyButton from './CopyButton.svelte';
	import Folded from './Folded.svelte';

	// The user listing, one row per user, mapping 1:1 onto what
	// `GET /api/v1/users` returns (spec 023 #8). Every number counts traces,
	// which is what the column headers say rather than leave to be guessed —
	// except `Sessions`, which counts sessions where they began.
	//
	// A row is a link to the user's page rather than a peek panel: the page is
	// a chart, two breakdowns and two tables, which is not a panel's worth of
	// screen (spec 008 #3 is about rows whose detail *is* panel-sized).

	let { rows }: { rows: UserRow[] } = $props();

	const numeric = 'px-3 py-1.5 text-right tabular-nums';

	// In a box narrower than the table the row is who and whether any of it
	// failed; what they sent, what it cost and when they were last seen fold
	// under the id (spec 006 #22). The number is the unfolded table's width
	// and its `min-width`.
	const fold = new Fold(944);
	const narrow = $derived(fold.narrow);
	const tokensText = (row: UserRow) => {
		const tokens = billedTokens(row.tokens);
		return tokens === null ? null : `${compact(tokens)} tokens`;
	};
</script>

<!-- The table scrolls inside its own box; the page never scrolls sideways
     (spec 006 #15), and folds in a box narrower than itself (#22). -->
<div bind:contentRect={fold.rect} class="min-h-0 flex-1 overflow-auto">
	<table class="w-full border-collapse text-left" style:min-width={fold.min}>
		<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
			<tr class="border-border border-b">
				<th scope="col" class={['px-3 py-2 font-medium', narrow && 'w-full']}>User</th>
				{#if !narrow}
					<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Traces</th>
					<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Sessions</th>
				{/if}
				<th scope="col" class={['px-3 py-2 font-medium', !narrow && 'w-28']}>Errors</th>
				{#if !narrow}
					<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Cost</th>
					<th scope="col" class="w-24 px-3 py-2 text-right font-medium">Tokens</th>
					<th scope="col" class="w-44 px-3 py-2 font-medium">First seen</th>
					<th scope="col" class="w-44 px-3 py-2 font-medium">Last seen</th>
				{/if}
			</tr>
		</thead>
		<tbody>
			{#each rows as row (row.user_id)}
				<tr class="border-border hover:bg-raised border-b transition-colors duration-100">
					<td class={['max-w-0 px-3 py-1.5', !narrow && 'min-w-48']}>
						<div class="flex min-w-0 items-center gap-1 font-mono">
							<!-- Cut in the middle, not at the end: two ids that share a
							     long prefix are told apart by their tails, and the whole
							     of it is the title and the button beside it (spec 023,
							     edge cases). -->
							<a
								href={href(`/users/${encodeURIComponent(row.user_id)}`)}
								title={row.user_id}
								class="truncate hover:underline"
							>
								{middleEllipsis(row.user_id)}
							</a>
							<CopyButton text={row.user_id} label="Copy the user id" />
						</div>
						{#if narrow}
							<div class="text-muted text-xs tabular-nums">
								<Folded
									values={[
										counted(row.traces, 'trace'),
										counted(row.sessions, 'session'),
										cost(row.total_cost),
										tokensText(row)
									]}
								/>
							</div>
							<div class="text-muted text-xs tabular-nums">
								<Folded
									values={[
										`last seen ${timestamp(row.last_seen)}`,
										`first seen ${timestamp(row.first_seen)}`
									]}
								/>
							</div>
						{/if}
					</td>
					{#if !narrow}
						<td class="text-muted {numeric}">{count(row.traces)}</td>
						<td class="text-muted {numeric}">{count(row.sessions)}</td>
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
						<td class="text-muted {numeric}" title={tokenClasses(row.tokens)}>
							{compact(billedTokens(row.tokens))}
						</td>
						<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
							{timestamp(row.first_seen)}
						</td>
						<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
							{timestamp(row.last_seen)}
						</td>
					{/if}
				</tr>
			{/each}
		</tbody>
	</table>
</div>
