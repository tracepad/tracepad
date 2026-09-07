<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import UsersIcon from '@lucide/svelte/icons/users';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type UserRow } from '$lib/api/client.svelte';
	import { readUserFilters, sortInForce, USER_SORTS, userSearch, type UserFilters } from '$lib/api/users';
	import Button from '$lib/components/Button.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import PaginationBar from '$lib/components/PaginationBar.svelte';
	import UserTable from '$lib/components/UserTable.svelte';
	import { count } from '$lib/format';
	import { asPage, Listing, UrlSpot } from '$lib/listing.svelte';
	import { freshSearch } from '$lib/page';

	// Users over `GET /api/v1/users` (spec 023 #8). The listing answers from
	// the rollup alone, so there is no live mode and nothing to tick: it
	// re-reads on a filter change and on Refresh, which is the whole of its
	// update story.
	//
	// A row is a link to the user's page rather than a peek panel — that page
	// is a chart, two breakdowns and two tables, which is more than a panel.

	const filters = $derived(readUserFilters(page.url.searchParams));
	const sort = $derived(sortInForce(filters));
	const filtering = $derived((filters.prefix ?? '') !== '');

	const listing = new Listing<UserRow>({
		key: () => userSearch(filters),
		spot: new UrlSpot(),
		read: async (at, counting, signal) => {
			const answer = await api.listUsers(filters, asPage(at, counting), signal);
			return { ...answer, rows: answer.users };
		},
		failed: 'Failed to read the users.'
	});

	/** A filter change starts at the first page, carrying the page size. */
	function navigate(next: UserFilters) {
		goto(`/users${freshSearch(userSearch(next), page.url.searchParams)}`, { keepFocus: true });
	}

	/** Commits the prefix on blur or Enter, never on every keystroke. */
	function commitPrefix(value: string) {
		const next = { ...filters };
		if (value.trim()) next.prefix = value.trim();
		else delete next.prefix;
		navigate(next);
	}
</script>

<svelte:head><title>Users · Tracepad</title></svelte:head>

<PageHeader title="Users">
	{#snippet meta()}
		{#if listing.loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else if listing.total}
			<span class="tabular-nums">{count(listing.total.value)}{listing.total.capped ? '+' : ''}</span>
		{:else}
			<span class="tabular-nums">{count(listing.rows.length)}</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button onclick={() => listing.reload()} busy={listing.loading} title="Read the listing again">
			<RefreshCw class="size-4" />
			Refresh
		</Button>
	{/snippet}
</PageHeader>

<div class="border-border overflow-x-auto border-b px-4 py-2">
	<div class="flex min-w-0 items-center gap-1.5">
		<label class="text-subtle text-sm" for="user-sort">Sort by</label>
		<select
			id="user-sort"
			value={sort}
			onchange={(event) => navigate({ ...filters, sort: event.currentTarget.value })}
			class="border-border bg-canvas rounded-md border px-2 py-1 text-sm"
		>
			{#each USER_SORTS as option (option.key)}
				<option value={option.key}>{option.label}</option>
			{/each}
		</select>
		<label class="sr-only" for="user-prefix">User id prefix</label>
		<input
			id="user-prefix"
			type="text"
			value={filters.prefix ?? ''}
			onchange={(event) => commitPrefix(event.currentTarget.value)}
			placeholder="User id starts with"
			autocomplete="off"
			spellcheck="false"
			class="border-border bg-canvas placeholder:text-subtle w-52 rounded-md border px-2 py-1 text-sm"
		/>
	</div>
</div>

{#if listing.problem}
	<p
		role="alert"
		class="text-danger bg-danger-soft border-border flex items-center gap-2 border-b px-4 py-2"
	>
		<TriangleAlert class="size-4 shrink-0" />
		{listing.problem}
	</p>
{/if}

{#if listing.rows.length > 0 || !listing.newest}
	<!-- The bar stays on an empty page that is not the first one, so a cursor
	     whose rows are gone still has a way back (PR #11 review). -->
	<UserTable rows={listing.rows} />
	<PaginationBar {...listing.bar} noun="user" />
	{#if listing.rows.length === 0 && !listing.loading}
		<p class="text-subtle flex flex-1 items-start justify-center p-8 text-center">
			Nothing on this page any more. Use « to go back to the first.
		</p>
	{/if}
{:else if !listing.loading && !listing.failure}
	<div class="flex flex-1 items-start justify-center overflow-auto p-8">
		<div class="max-w-lg">
			{#if filtering}
				<h2 class="font-medium">No user id starts with that</h2>
				<p class="text-muted mt-1">
					The prefix is case-sensitive and is in the URL, so this is a link you can share — or
					clear.
				</p>
				<Button class="mt-3" onclick={() => navigate({ sort: filters.sort })}>Clear the prefix</Button>
			{:else}
				<!-- Never "there are no users": this listing is the rollup, and a
				     user first seen minutes ago is not in it yet (spec 023 #4). The
				     link is where those traces already are. -->
				<h2 class="flex items-center gap-2 font-medium">
					<UsersIcon class="text-subtle size-4" />
					No user has been seen yet
				</h2>
				<p class="text-muted mt-1">
					A user is any trace carrying <code class="font-mono">user.id</code> or
					<code class="font-mono">langfuse.user.id</code>. Set it on your traces — in our SDK that is
					<code class="font-mono">update_trace(user_id=…)</code> — and every account shows up here as
					one row.
				</p>
				<p class="text-muted mt-2">
					This listing is built from an hourly roll-up and trails live traffic by a few minutes, so a
					user first seen just now is not on it yet. The
					<a href="/traces" class="text-accent hover:underline">Traces</a> screen filters by user id
					and is live.
				</p>
			{/if}
		</div>
	</div>
{:else}
	<div class="flex-1"></div>
{/if}
