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
	// down a column of rows swaps the panel in place. Below `lg` it covers the
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

	// True exactly when the panel covers the viewport rather than sitting
	// beside the listing (#5): "beside" needs room for both, and below `lg`
	// there is none.
	const narrow = new MediaQuery('(max-width: 63.99rem)');
	const still = new MediaQuery('(prefers-reduced-motion: reduce)');
	// Motion on a state change, inside the 100–200 ms band of spec 006 #6;
	// reduced motion makes it an instant swap rather than a slower one (#11).
	const slide = $derived(still.current ? 0 : 160);

	let panel = $state<HTMLElement | null>(null);

	// Where focus goes when the panel closes: the row it is showing. Captured
	// at init — the listing focuses its row link on the way in, so there is
	// something to come back to — and kept current as the panel walks. The
	// panel is not remounted between rows, so a once-captured opener would
	// send the reader back to the row they started the scan from rather than
	// the one they are looking at (PR #10, second review).
	let opener = typeof document === 'undefined' ? null : document.activeElement;

	$effect(() => {
		// `fullHref` is what changes when the panel moves to another row.
		void fullHref;
		// The listing marks its open row `aria-current="true"`; the sidebar's
		// own current link is `aria-current="page"` and does not match.
		//
		// Scoped out of the panel's own subtree: a listing inside it — the
		// session kept mounted under a drilled trace — marks a row too, and
		// that one is `display:none`. Focusing a hidden element is a silent
		// no-op, so taking it would drop the reader on the body, which is the
		// very thing this is here to prevent (PR #10, third review).
		const lit = [...document.querySelectorAll('a[aria-current="true"]')].find(
			(element) => !panel?.contains(element)
		);
		if (lit) opener = lit;
	});

	onMount(() => {
		panel?.focus();
		return () => {
			// The body is not an answer: focusing it would move the reader
			// nowhere and lose the place they were in.
			if (opener instanceof HTMLElement && opener !== document.body && opener.isConnected) {
				opener.focus();
			}
		};
	});

	/**
	 * Escape closes and `j`/`k` walk the rows, from anywhere rather than only
	 * from inside the panel: with the listing still live under it (#4), the
	 * last thing clicked is often a row and focus is out there.
	 * `defaultPrevented` leaves the key to whatever handled it first — a
	 * popover or a dialog closing itself.
	 */
	function onkeydown(event: KeyboardEvent) {
		if (event.defaultPrevented) return;
		// Escape closes from anywhere, a text field included. The second
		// review round moved it behind the `typing` guard on the reasoning
		// that in a filter field Escape means "abandon this edit" — but
		// nothing in this interface implements that and browsers do not do it
		// for a plain input, so the guard left Escape meaning *nothing* there
		// and took away the only key that closes the panel. Restored, and the
		// promise not made (PR #10, third review).
		if (event.key === 'Escape') {
			onclose();
			return;
		}
		// A letter, on the other hand, is only a shortcut where a letter is
		// not being typed, and never as part of a browser combination.
		if (event.metaKey || event.ctrlKey || event.altKey || event.shiftKey) return;
		if (typing(event.target)) return;
		// Gated on the handler as well as on the end of the listing: a layer
		// with nothing to walk (spec 008 #9) has no controls on screen, so a
		// swallowed key would have nothing to explain itself with.
		if (event.key === 'j' && onnext && hasNext) onnext();
		else if (event.key === 'k' && onprev && hasPrev) onprev();
		else return;
		event.preventDefault();
	}

	function typing(target: EventTarget | null): boolean {
		if (!(target instanceof HTMLElement)) return false;
		return (
			target.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(target.tagName)
		);
	}

	/** Below `lg` the rest of the page is covered, so Tab does not go there. */
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

	// The two that carry a shortcut are wider than a square, because the key
	// is drawn on them.
	const walk =
		'text-muted hover:bg-raised hover:text-fg pointer-coarse:h-11 pointer-coarse:px-3 ' +
		'inline-flex h-7 shrink-0 cursor-pointer items-center gap-1 rounded-md px-1.5 ' +
		'transition-colors duration-100 disabled:cursor-default disabled:opacity-40 ' +
		'disabled:hover:bg-transparent';
	/** The key as a key: a keycap, not a letter floating next to an icon. */
	const cap =
		'border-border bg-raised pointer-coarse:hidden rounded border px-1 font-mono text-[10px] ' +
		'leading-4 uppercase';
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
		flex-col border-l outline-none lg:w-[clamp(34rem,calc(100vw-30rem),60rem)]"
>
	<header class="border-border flex h-12 shrink-0 items-center gap-2 border-b pr-2 pl-3">
		{@render title()}
		{#if meta}
			<div class="text-subtle flex min-w-0 items-center gap-2 text-sm">{@render meta()}</div>
		{/if}

		<div class="ml-auto flex items-center gap-0.5">
			{#if onprev || onnext}
				<!-- Disabled rather than hidden at the ends: a control that
				     vanishes moves everything beside it, and the dimmed key is
				     how the panel says the listing has run out (spec 008 #13). -->
				<button
					type="button"
					class={walk}
					disabled={!hasPrev}
					aria-label="Previous row"
					aria-keyshortcuts="k"
					title="Previous row (k)"
					onclick={() => onprev?.()}
				>
					<ChevronUp class="size-4" />
					<kbd class={cap}>k</kbd>
				</button>
				<button
					type="button"
					class={walk}
					disabled={!hasNext}
					aria-label="Next row"
					aria-keyshortcuts="j"
					title="Next row (j)"
					onclick={() => onnext?.()}
				>
					<ChevronDown class="size-4" />
					<kbd class={cap}>j</kbd>
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
