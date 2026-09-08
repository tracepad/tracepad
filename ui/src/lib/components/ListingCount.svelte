<script lang="ts" generics="Row">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { count } from '$lib/format';
	import type { Listing } from '$lib/listing.svelte';

	// The number a listing's header carries (spec 026 #2), written out six
	// times: the spinner while the page is in flight, the capped total once it
	// lands, and otherwise what is on screen — which is still a number, and an
	// empty slot is not (PR #11 review).
	//
	// A listing that asks for no count at all (`count: false`) leaves `total`
	// at null for the life of the page, so the third branch is the only one it
	// ever reaches — which is exactly what its own two-branch copy printed.

	let { listing }: { listing: Listing<Row> } = $props();
</script>

{#if listing.loading}
	<LoaderCircle class="size-3.5 animate-spin" />
{:else if listing.total}
	<span class="tabular-nums">{count(listing.total.value)}{listing.total.capped ? '+' : ''}</span>
{:else}
	<!-- The count has not landed, or could not be taken. -->
	<span class="tabular-nums">{count(listing.rows.length)}</span>
{/if}
