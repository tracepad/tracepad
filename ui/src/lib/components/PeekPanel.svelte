<script lang="ts">
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import ChevronUp from '@lucide/svelte/icons/chevron-up';
	import Maximize2 from '@lucide/svelte/icons/maximize-2';
	import X from '@lucide/svelte/icons/x';
	import type { Snippet } from 'svelte';
	import { onMount } from 'svelte';
	import { MediaQuery } from 'svelte/reactivity';
	import { cubicOut } from 'svelte/easing';
	import { fly } from 'svelte/transition';

	// The panel a listing opens a row in (spec 008). It is chrome only: a
	// header of controls over whatever body the caller renders, which is the
	// same component the full page renders (#8).
	//
	// On a wide screen it is deliberately *not* modal (#4): no scrim, no
	// scroll lock, and the listing underneath stays clickable so that walking
	// down a column of rows swaps the panel in place. Below `md` it covers the
	// viewport, and there — where the listing is genuinely unreachable — it
	// says so and holds the Tab key inside itself (#12).

	let {
		label,
		onclose,
		onprev,
		onnext,
		hasPrev = false,
		hasNext = false,
		fullHref,
		fullLabel,
		title,
		meta,
		children
	}: {
		/** Names the dialog for anybody who cannot see its heading. */
		label: string;
		onclose: () => void;
		/** Left out entirely by a layer that has no neighbours to walk (#9). */
		onprev?: () => void;
		onnext?: () => void;
		hasPrev?: boolean;
		hasNext?: boolean;
		/** Where ⤢ leads: the canonical, shareable page for what is shown. */
		fullHref: string;
		fullLabel: string;
		title: Snippet;
		meta?: Snippet;
		children: Snippet;
	} = $props();

	// The `md` breakpoint of the design system, from the other side: this is
	// true exactly when the panel covers the viewport.
	const narrow = new MediaQuery('(max-width: 47.99rem)');
	const still = new MediaQuery('(prefers-reduced-motion: reduce)');
	// Motion on a state change, inside the 100–200 ms band of spec 006 #6;
	// reduced motion makes it an instant swap rather than a slower one (#11).
	const slide = $derived(still.current ? 0 : 160);

	let panel = $state<HTMLElement | null>(null);

	// Whatever was focused when the panel opened is where focus goes back to,
	// which is the row that opened it.
	const opener = typeof document === 'undefined' ? null : document.activeElement;

	onMount(() => {
		panel?.focus();
		return () => {
			if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
		};
	});

	/**
	 * Escape closes from anywhere, not only from inside the panel: with the
	 * listing still live under it (#4), the last thing clicked is often a row
	 * and focus is out there. `defaultPrevented` leaves the key to whatever
	 * handled it first — a popover or a dialog closing itself.
	 */
	function onkeydown(event: KeyboardEvent) {
		if (event.key === 'Escape' && !event.defaultPrevented) onclose();
	}

	/** Below `md` the rest of the page is covered, so Tab does not go there. */
	function contain(event: KeyboardEvent) {
		if (event.key !== 'Tab' || !narrow.current || !panel) return;
		const stops = [...panel.querySelectorAll<HTMLElement>(FOCUSABLE)].filter(
			(element) => element.offsetParent !== null
		);
		const first = stops[0];
		const last = stops[stops.length - 1];
		if (!first) return;
		const active = document.activeElement;
		if (event.shiftKey && (active === first || active === panel)) {
			last.focus();
			event.preventDefault();
		} else if (!event.shiftKey && active === last) {
			first.focus();
			event.preventDefault();
		}
	}

	const FOCUSABLE =
		'a[href], button:not([disabled]), input:not([disabled]), select, textarea, ' +
		'[tabindex]:not([tabindex="-1"])';

	const control =
		'text-muted hover:bg-raised hover:text-fg pointer-coarse:size-11 inline-flex size-7 ' +
		'shrink-0 cursor-pointer items-center justify-center rounded-md transition-colors ' +
		'duration-100 disabled:cursor-default disabled:opacity-40 disabled:hover:bg-transparent';
</script>

<svelte:window {onkeydown} />

<!-- A `div`, not an `aside`: `role="dialog"` on a landmark element is a
     contradiction the compiler is right to refuse. -->
<div
	bind:this={panel}
	role="dialog"
	aria-modal={narrow.current}
	aria-label={label}
	tabindex="-1"
	onkeydown={contain}
	transition:fly={{ x: '100%', duration: slide, easing: cubicOut, opacity: 1 }}
	class="border-border bg-canvas shadow-overlay fixed inset-y-0 right-0 z-40 flex w-full
		flex-col border-l outline-none md:w-[min(60rem,72vw)]"
>
	<header class="border-border flex h-12 shrink-0 items-center gap-2 border-b pr-2 pl-3">
		{@render title()}
		{#if meta}
			<div class="text-subtle flex min-w-0 items-center gap-2 text-sm">{@render meta()}</div>
		{/if}

		<div class="ml-auto flex items-center gap-0.5">
			{#if onprev || onnext}
				<!-- Disabled rather than hidden at the ends: a control that
				     vanishes moves everything beside it (spec 008, a11y floor). -->
				<button
					type="button"
					class={control}
					disabled={!hasPrev}
					aria-label="Previous row"
					onclick={() => onprev?.()}
				>
					<ChevronUp class="size-4" />
				</button>
				<button
					type="button"
					class={control}
					disabled={!hasNext}
					aria-label="Next row"
					onclick={() => onnext?.()}
				>
					<ChevronDown class="size-4" />
				</button>
			{/if}
			<a href={fullHref} class={control} aria-label={fullLabel} title={fullLabel}>
				<Maximize2 class="size-4" />
			</a>
			<button type="button" class={control} aria-label="Close the panel" onclick={onclose}>
				<X class="size-4" />
			</button>
		</div>
	</header>

	<div class="flex min-h-0 flex-1 flex-col overflow-hidden">{@render children()}</div>
</div>
