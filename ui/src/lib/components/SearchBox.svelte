<script lang="ts">
	import SearchIcon from '@lucide/svelte/icons/search';
	import X from '@lucide/svelte/icons/x';

	// The search box (spec 011, Application contract). First-class on the bar
	// rather than inside the filter popover: it is the filter people arrive
	// with, not one they go looking for.

	let { value, onchange }: { value: string; onchange: (q: string) => void } = $props();

	// The box edits a draft and commits on Enter or on blur, never on a
	// keystroke: a listing that re-queried on every letter would fight the
	// person typing (the convention of spec 007, and the same reason the
	// filter popover edits a copy).
	//
	// Held as "what has been typed since the last commit, if anything" rather
	// than as a copy kept in step by an effect: with nothing typed the box *is*
	// the URL, so a chip cleared, the back button and a link opened all reach it
	// without a second write and a second render.
	let typed = $state<string | null>(null);
	const draft = $derived(typed ?? value);

	function commit() {
		const next = draft.trim();
		typed = null;
		if (next !== value) onchange(next);
	}

	function clear() {
		typed = null;
		if (value !== '') onchange('');
	}
</script>

<form
	class="flex min-w-0 items-center"
	onsubmit={(event) => {
		event.preventDefault();
		commit();
	}}
>
	<div class="relative flex min-w-0 items-center">
		<SearchIcon class="text-subtle pointer-events-none absolute left-2 size-4" />
		<input
			type="search"
			name="q"
			aria-label="Search prompts, answers and errors"
			placeholder="Search prompts, answers, errors…"
			value={draft}
			oninput={(event) => (typed = event.currentTarget.value)}
			onblur={commit}
			autocomplete="off"
			spellcheck="false"
			class="border-border bg-canvas placeholder:text-subtle pointer-coarse:min-h-11 w-40 rounded-md
				border py-1 pr-7 pl-7 text-sm transition-[width] duration-150 focus:w-64 sm:w-56 sm:focus:w-80
				[&::-webkit-search-cancel-button]:hidden"
		/>
		{#if draft}
			<!-- Its own control, because the box is on the bar in plain sight and
			     the filter popover's "Clear all" deliberately leaves it alone.
			     `mousedown` is swallowed so the box never blurs: a blur here
			     commits, and committing what was just typed re-derives `draft`
			     from the URL, which unmounts this button before its own click is
			     dispatched — so ✕ ran the search it was pressed to discard
			     (found in review of PR #16). With a search already on, it cost
			     two navigations instead of one. -->
			<button
				type="button"
				onmousedown={(event) => event.preventDefault()}
				onclick={clear}
				aria-label="Clear the search box"
				class="text-subtle hover:text-fg absolute right-1.5 cursor-pointer p-0.5"
			>
				<X class="size-3.5" />
			</button>
		{/if}
	</div>
</form>
