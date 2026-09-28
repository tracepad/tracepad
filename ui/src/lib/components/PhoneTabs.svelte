<script lang="ts">
	import Ellipsis from '@lucide/svelte/icons/ellipsis';
	import X from '@lucide/svelte/icons/x';
	import { Dialog } from 'bits-ui';
	import { cubicOut } from 'svelte/easing';
	import { MediaQuery } from 'svelte/reactivity';
	import { fade, fly } from 'svelte/transition';
	import { page } from '$app/state';
	import { api } from '$lib/api/client.svelte';
	import { href, switcher } from '$lib/project.svelte';
	import { active, isGroup, ITEMS, SECTIONS, type Group, type Item } from '$lib/sections';
	import { swipeDown } from '$lib/swipe';
	import AccountMenu from './AccountMenu.svelte';
	import NavList from './NavList.svelte';
	import ProjectSwitcher from './ProjectSwitcher.svelte';
	import ThemeToggle from './ThemeToggle.svelte';

	// The phone's navigation (spec 006 #20): a 48px bar on top for the
	// product, the project and the account, and a tab bar under the thumb —
	// the four screens a page at night opens, and *More* for the rest in a
	// sheet. The listing gets the height between them.

	/** The screens a tab holds; everything else is in *More*. */
	const TABS = ['/dashboard', '/traces', '/sessions', '/users'];

	const tabs = ITEMS.filter((item) => TABS.includes(item.href));
	/** The column without the tabbed screens, its group kept. */
	const rest = SECTIONS.flatMap((section): (Item | Group)[] => {
		if (!isGroup(section)) return TABS.includes(section.href) ? [] : [section];
		return [{ ...section, children: section.children.filter((c) => !TABS.includes(c.href)) }];
	});

	let open = $state(false);

	const still = new MediaQuery('(prefers-reduced-motion: reduce)');
	const slide = $derived(still.current ? 0 : 200);
	/**
	 * *More* is lit while one of its screens is on show, and keeps its name:
	 * a tab renamed after the screen reads as a fifth destination.
	 */
	const tucked = $derived(ITEMS.some((item) => !TABS.includes(item.href) && active(item.href)));

	// A link followed is the sheet's job done.
	$effect(() => {
		void page.url.pathname;
		open = false;
	});

	const tab =
		'flex min-h-14 flex-col items-center justify-center gap-0.5 text-xs transition-colors duration-100';
	const pill = 'flex h-7 w-12 items-center justify-center rounded-full transition-colors duration-100';
</script>

{#snippet face(Glyph: Item['icon'], label: string, lit: boolean)}
	<span class={[pill, lit && 'bg-accent-soft']}><Glyph class="size-5" /></span>
	{label}
{/snippet}

<header class="border-border bg-surface flex h-12 shrink-0 items-center gap-2 border-b pr-1 pl-3">
	<span class="shrink-0 text-lg font-semibold tracking-tight">Tracepad</span>
	<div class="flex min-w-0 flex-1 items-center">
		<ProjectSwitcher bind:open={switcher.open} />
	</div>
	<AccountMenu />
</header>

<!-- Under the page, visually: `order-last` puts the bar below `main` in the
     shell's column, while the document keeps the navigation before the
     content, where a screen reader's landmarks expect it. -->
<nav
	aria-label="Sections"
	class="border-border bg-surface order-last grid shrink-0 grid-cols-5 border-t"
>
	{#each tabs as item (item.href)}
		{@const lit = active(item.href)}
		<a
			href={href(item.href)}
			aria-current={lit ? 'page' : undefined}
			class={[tab, lit ? 'text-accent font-medium' : 'text-muted hover:text-fg']}
		>
			{@render face(item.icon, item.label, lit)}
		</a>
	{/each}

	<Dialog.Root bind:open>
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
			<Dialog.Content forceMount>
				{#snippet child({ props, open: shown })}
					{#if shown}
						<!-- A sheet from the bottom, closed by Escape, the backdrop,
						     the close button or a swipe down from its grip. -->
						<div
							{...props}
							{@attach swipeDown(() => (open = false), '[data-grip]')}
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
								<NavList sections={rest} />
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
