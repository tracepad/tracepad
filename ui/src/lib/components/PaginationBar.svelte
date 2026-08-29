<script lang="ts">
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import ChevronsLeft from '@lucide/svelte/icons/chevrons-left';
	import ChevronsRight from '@lucide/svelte/icons/chevrons-right';
	import { PAGE_SIZES } from '$lib/page';
	import { count as formatCount } from '$lib/format';

	// The bar under every listing (spec 009 #8): how big a page is, how much is
	// on it, and the four ways to move.
	//
	// What it deliberately does *not* show is a page number. Keyset pagination
	// knows where a page sits only as a cursor, never as an ordinal — the same
	// fact that makes "of N" cost an offset scan (#3). So the bar says how many
	// rows are on this page and how many the filters match, which is what the
	// reader was actually asking.

	let {
		limit,
		rows,
		total = null,
		hasPrev,
		hasNext,
		atNewest,
		atOldest,
		onresize,
		onfirst,
		onprev,
		onnext,
		onlast,
		noun
	}: {
		limit: number;
		/** How many rows this page carries. */
		rows: number;
		/** The capped count, when the screen has asked for one. */
		total?: { value: number; capped: boolean } | null;
		/** Whether a *step* exists either way: the two cursors of this page. */
		hasPrev: boolean;
		hasNext: boolean;
		/**
		 * Whether this page already *is* an end. « and » are anchors, not
		 * steps — they need no cursor to reach, so they stay live on a page
		 * that has none, which is exactly the page somebody is stranded on
		 * when the rows a cursor named are gone (PR #11, second review).
		 */
		atNewest: boolean;
		atOldest: boolean;
		onresize: (limit: number) => void;
		onfirst: () => void;
		onprev: () => void;
		onnext: () => void;
		onlast: () => void;
		/** "trace" / "session": what the numbers are counting. */
		noun: string;
	} = $props();

	// Built here rather than interpolated across the markup: Svelte keeps the
	// newlines between `{...}` blocks, and a caption split over three text
	// nodes is one nothing can be asserted about.
	const label = $derived.by(() => {
		const plural = (n: number, capped = false) => (n === 1 && !capped ? noun : `${noun}s`);
		if (total === null) return `${formatCount(rows)} ${plural(rows)}`;
		const matching = formatCount(total.value) + (total.capped ? '+' : '');
		return `${formatCount(rows)} of ${matching} ${plural(total.value, total.capped)}`;
	});

	// The steps on offer, plus whatever size the URL actually carries: the
	// API takes anything from 1 to 500, and a select that cannot show the
	// current value is a control that lies about the state it is in.
	const sizes = $derived(
		PAGE_SIZES.includes(limit as (typeof PAGE_SIZES)[number])
			? [...PAGE_SIZES]
			: [...PAGE_SIZES, limit].sort((a, b) => a - b)
	);

	const step =
		'text-muted hover:bg-raised hover:text-fg pointer-coarse:size-11 inline-flex size-7 ' +
		'shrink-0 cursor-pointer items-center justify-center rounded-md transition-colors ' +
		'duration-100 disabled:cursor-default disabled:opacity-40 disabled:hover:bg-transparent';
</script>

<div
	class="border-border flex shrink-0 flex-wrap items-center justify-between gap-x-4 gap-y-2
		border-t px-4 py-2"
>
	<label class="text-subtle flex items-center gap-2 text-xs">
		Rows per page
		<select
			value={limit}
			onchange={(event) => onresize(Number(event.currentTarget.value))}
			class="border-border bg-canvas text-fg rounded-md border px-1.5 py-1 text-sm tabular-nums"
		>
			{#each sizes as size (size)}
				<option value={size}>{size}</option>
			{/each}
		</select>
	</label>

	<p class="text-subtle text-xs tabular-nums">{label}</p>

	<div class="flex items-center gap-0.5">
		<!-- Both ends are a page like any other: keyset reads the index from
		     either side, so "the newest" and "the oldest" cost what "the next"
		     costs (spec 009 #2). -->
		<button
			type="button"
			class={step}
			disabled={atNewest}
			aria-label="Newest page"
			title="Newest page"
			onclick={onfirst}
		>
			<ChevronsLeft class="size-4" />
		</button>
		<button
			type="button"
			class={step}
			disabled={!hasPrev}
			aria-label="Previous page"
			title="Previous page"
			onclick={onprev}
		>
			<ChevronLeft class="size-4" />
		</button>
		<button
			type="button"
			class={step}
			disabled={!hasNext}
			aria-label="Next page"
			title="Next page"
			onclick={onnext}
		>
			<ChevronRight class="size-4" />
		</button>
		<button
			type="button"
			class={step}
			disabled={atOldest}
			aria-label="Oldest page"
			title="Oldest page"
			onclick={onlast}
		>
			<ChevronsRight class="size-4" />
		</button>
	</div>
</div>
