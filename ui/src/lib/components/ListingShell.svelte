<script lang="ts" generics="Row">
	import type { Snippet } from 'svelte';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import type { Listing, Total } from '$lib/listing.svelte';
	import PaginationBar from './PaginationBar.svelte';

	// What every listing writes out *around* the loader (spec 026 #1): the
	// failure line, the table, the bar, the page a cursor found empty, and the
	// empty state. Spec 010 held the loading; this holds the ladder over it,
	// which eleven files carried line for line — with the same comment about
	// the same review — until the interface had fifteen listings and a bug in
	// any of those branches was eleven bugs.
	//
	// The two variable parts are snippets rather than props because a table and
	// an empty state are markup: the shell must not know a `TraceTable` from a
	// `SessionTable`, and a caller's table closes over whatever else its page
	// has (a filter, a selection, a busy row).

	let {
		listing,
		noun,
		table,
		empty,
		total,
		problem,
		back = 'newest'
	}: {
		listing: Listing<Row>;
		/** "trace" / "session": what the bar's numbers are counting. */
		noun: string;
		table: Snippet;
		/**
		 * What to draw when the listing is empty and nobody is on a stale
		 * cursor. Optional: a tab inside a page may have none, and then the
		 * shell renders the spacer (Edge cases).
		 */
		empty?: Snippet;
		/**
		 * The bar's count, when the caller knows a better one than the loader
		 * does — an exact `trace_count`, a dataset's `item_count` (spec 010,
		 * divergence 6). Left out, the loader's own capped count stands.
		 */
		total?: Total | null;
		/**
		 * What the failure line says, when the page has a second failure of its
		 * own to fold into it (a queue's writes) or handles failure itself and
		 * wants none here. Left out, it is the loader's `problem`.
		 */
		problem?: string | null;
		/**
		 * Where « goes back to, in this listing's own words: the newest page of
		 * a listing read newest first, the first page of one read by name or by
		 * `seq`.
		 */
		back?: 'newest' | 'first';
	} = $props();

	const banner = $derived(problem === undefined ? listing.problem : problem);
	const counted = $derived(total === undefined ? listing.bar.total : total);
</script>

{#if banner}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{banner}
	</p>
{/if}

{#if listing.rows.length > 0 || !listing.newest}
	<!-- The bar stays on an empty page that is not the first one: a cursor
	     whose rows are gone — swept by retention, say — would otherwise leave
	     no way back to the listing but editing the URL (PR #11 review). -->
	{@render table()}
	<PaginationBar {...listing.bar} total={counted} {noun} />
	{#if listing.rows.length === 0 && !listing.loading}
		<p class="text-subtle flex flex-1 items-start justify-center p-8 text-center">
			Nothing on this page any more. Use « to go back to the {back}.
		</p>
	{/if}
{:else if !listing.loading && !listing.failure && empty}
	{@render empty()}
{:else}
	<!-- Nothing to say yet, or nothing that is this component's to say: the
	     spacer keeps the bar and the panel where they were. -->
	<div class="flex-1"></div>
{/if}
