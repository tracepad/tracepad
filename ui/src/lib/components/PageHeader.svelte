<script lang="ts">
	import type { Snippet } from 'svelte';

	// The one horizontal rule between the shell and a screen. Every page wears
	// the same 48px bar so that switching screens never moves the content down
	// or up a few pixels.
	//
	// Forty-eight is a floor on a phone rather than a fixed height (spec 026
	// #5). A row that cannot wrap has one way to fit a phone, which is to
	// squeeze the meta until the thing the reader came for — a score's name, a
	// user's id — is an ellipsis beside a full-width button. So the title and
	// the meta wrap as a pair and the actions sit outside them: what runs out
	// of room is the meta, which drops to a second line under the title, and
	// never the controls. Two lines at most, because the pair has two members
	// and the meta does not wrap inside itself; one line when there is no meta.
	//
	// Only under `sm` (#14). Flexbox wraps on what the items would like to be,
	// not on what they would shrink to, so a wrap that is always on takes a
	// long queue description to a second line on a desktop too — where the bar
	// was 48px and the description was an ellipsis, and where nothing about
	// the screen is short of room. Above the breakpoint the row is the row it
	// always was.
	//
	// `stackBelow` (spec 006 #27): under 1,360 px — the only value — the actions
	// take a row of their own, and the title and the meta wrap as a pair.
	let {
		title,
		meta,
		actions,
		stackBelow
	}: { title: string; meta?: Snippet; actions?: Snippet; stackBelow?: 1360 } = $props();
	const stack = $derived(stackBelow === 1360);
</script>

<header
	class={[
		'border-border flex min-h-12 shrink-0 items-center gap-3 border-b px-4',
		stack && 'flex-wrap gap-y-1 py-1.5 min-[1360px]:flex-nowrap min-[1360px]:py-0'
	]}
>
	<div
		class={[
			'flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1',
			stack ? 'min-[1360px]:flex-nowrap' : 'sm:flex-nowrap'
		]}
	>
		<!-- The title truncates before anything moves: a long queue name is not
		     a reason to push the two verbs of the screen off the edge. -->
		<h1 class="min-w-0 truncate text-lg font-semibold tracking-tight">{title}</h1>
		{#if meta}
			<div class={['text-subtle flex min-w-0 items-center gap-2 text-sm', stack && 'flex-wrap gap-y-0.5']}>
				{@render meta()}
			</div>
		{/if}
	</div>
	{#if actions}
		<div class={['flex items-center gap-1.5', stack && 'w-full justify-end min-[1360px]:w-auto']}>
			{@render actions()}
		</div>
	{/if}
</header>
