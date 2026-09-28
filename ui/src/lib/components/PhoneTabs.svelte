<script lang="ts">
	import Ellipsis from '@lucide/svelte/icons/ellipsis';
	import X from '@lucide/svelte/icons/x';
	import { Dialog } from 'bits-ui';
	import { cubicOut } from 'svelte/easing';
	import { MediaQuery } from 'svelte/reactivity';
	import { fade, fly } from 'svelte/transition';
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { api } from '$lib/api/client.svelte';
	import { STILL } from '$lib/phone';
	import { href } from '$lib/project.svelte';
	import { active, ITEMS, MORE, TABS, type Item } from '$lib/sections';
	import { swipeDown } from '$lib/swipe';
	import NavList from './NavList.svelte';
	import ThemeToggle from './ThemeToggle.svelte';

	// A phone's tabs, under the page (spec 006 #20): the sections marked `tab`,
	// and *More* for the rest in a sheet. After `main` in the document as on
	// the screen, so Tab reaches the page before the bar.

	let open = $state(false);
	/**
	 * Closed by leaving for another screen, whose own focus comes first: the
	 * sheet must not hand it back to *More* when it finishes closing. Escape,
	 * the backdrop and the close button do return it there.
	 */
	let leaving = false;

	function leave() {
		leaving = true;
		open = false;
	}

	const still = new MediaQuery(STILL);
	const slide = $derived(still.current ? 0 : 200);
	/**
	 * *More* is lit while one of its screens is on show, and keeps its name:
	 * a tab renamed after the screen reads as a fifth destination.
	 */
	const tucked = $derived(ITEMS.some((item) => !item.tab && active(item.href)));

	// Another screen is the sheet's job done, however it was reached. The
	// path, not the URL: a screen that rewrites its own query — a page turned,
	// a filter — is still the same screen, and must not shut a sheet open over it.
	const path = $derived(page.url.pathname);
	$effect(() => {
		void path;
		untrack(() => open && leave());
	});

	const tab =
		'flex min-h-14 flex-col items-center justify-center gap-0.5 text-xs transition-colors duration-100';
	const pill = 'flex h-7 w-12 items-center justify-center rounded-full transition-colors duration-100';
</script>

{#snippet face(Glyph: Item['icon'], label: string, lit: boolean)}
	<span class={[pill, lit && 'bg-accent-soft']}><Glyph class="size-5" /></span>
	{label}
{/snippet}

<nav
	aria-label="Sections"
	class="border-border bg-surface grid shrink-0 border-t"
	style:grid-template-columns="repeat({TABS.length + 1}, minmax(0, 1fr))"
>
	{#each TABS as item (item.href)}
		{@const lit = active(item.href)}
		<a
			href={href(item.href)}
			aria-current={lit ? 'page' : undefined}
			class={[tab, lit ? 'text-accent font-medium' : 'text-muted hover:text-fg']}
		>
			{@render face(item.icon, item.label, lit)}
		</a>
	{/each}

	<Dialog.Root bind:open onOpenChange={(next) => next && (leaving = false)}>
		<Dialog.Trigger
			aria-current={tucked ? 'true' : undefined}
			class={[tab, tucked ? 'text-accent font-medium' : 'text-muted hover:text-fg']}
		>
			{@render face(Ellipsis, 'More', tucked)}
		</Dialog.Trigger>
		<Dialog.Portal>
			<Dialog.Overlay forceMount>
				{#snippet child({ props, open: shown })}
					{#if shown}
						<div
							{...props}
							transition:fade={{ duration: slide }}
							class="fixed inset-0 z-50 bg-black/50"
						></div>
					{/if}
				{/snippet}
			</Dialog.Overlay>
			<Dialog.Content
				forceMount
				onCloseAutoFocus={(event) => leaving && event.preventDefault()}
			>
				{#snippet child({ props, open: shown })}
					{#if shown}
						<!-- A sheet from the bottom, closed by Escape, the backdrop,
						     the close button, a link or a swipe down from its grip. -->
						<div
							{...props}
							{@attach swipeDown(() => (open = false), '[data-grip]', still.current)}
							transition:fly={{ y: '100%', duration: slide, easing: cubicOut, opacity: 1 }}
							class="border-border bg-surface shadow-overlay fixed inset-x-0 bottom-0 z-50 flex
								max-h-[85dvh] flex-col rounded-t-xl border-t outline-none"
						>
							<div data-grip class="shrink-0 touch-none">
								<div class="bg-border-strong mx-auto mt-2 h-1 w-9 rounded-full"></div>
								<div class="flex h-11 items-center gap-2 pr-1.5 pl-4">
									<Dialog.Title class="text-lg font-semibold tracking-tight">More</Dialog.Title>
									<Dialog.Close
										aria-label="Close"
										class="text-muted hover:bg-raised hover:text-fg ml-auto inline-flex size-11
											items-center justify-center rounded-md"
									>
										<X class="size-5" />
									</Dialog.Close>
								</div>
							</div>
							<nav aria-label="More sections" class="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
								<NavList sections={MORE} touch onnavigate={leave} />
							</nav>
							<div class="border-border flex shrink-0 items-center gap-2 border-t px-3 py-1.5">
								<span class="text-subtle font-mono text-xs" title="Server version">
									{api.version ?? ''}
								</span>
								<span class="ml-auto"><ThemeToggle /></span>
							</div>
						</div>
					{/if}
				{/snippet}
			</Dialog.Content>
		</Dialog.Portal>
	</Dialog.Root>
</nav>
