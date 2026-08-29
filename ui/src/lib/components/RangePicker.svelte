<script lang="ts">
	import CalendarDays from '@lucide/svelte/icons/calendar-days';
	import X from '@lucide/svelte/icons/x';
	import { DateRangePicker, Portal } from 'bits-ui';
	import {
		fromDate,
		getLocalTimeZone,
		toCalendarDate,
		type CalendarDate,
		type DateValue
	} from '@internationalized/date';
	import { PRESETS, matchPreset, type Range } from '$lib/api/range';
	import { timestamp } from '$lib/format';
	import Button from './Button.svelte';

	// The one time-window control in the app (spec 007 #7). Stats and the
	// Traces filter bar both use it, so "the last 24 hours" cannot come to mean
	// two different things on two screens.
	//
	// The presets are what somebody reaches for daily; the calendar answers
	// "what happened last Tuesday" without hand-typing an RFC 3339 timestamp.
	// A preset sets `from` and leaves the end open, because the window it names
	// keeps ending now.

	let { range, onchange }: { range: Range; onchange: (next: Range) => void } = $props();

	const zone = getLocalTimeZone();

	// The clock is read when the window changes, not when the page was opened.
	// `new Date()` is not reactive, so this recomputes on a new `range` and
	// never on its own — the trigger cannot relabel itself under a reader, and
	// a preset pressed on a page that has been open for an hour still matches
	// the preset that set it. The two clocks have to agree: the buttons resolve
	// `from` against the live one, and `matchPreset` allows a minute either way.
	const preset = $derived(matchPreset(range, new Date()));
	const label = $derived(
		preset
			? PRESETS.find((candidate) => candidate.key === preset)!.label
			: range.from || range.to
				? `${range.from ? timestamp(range.from) : 'Anything'} → ${range.to ? timestamp(range.to) : 'now'}`
				: 'Any time'
	);

	/**
	 * The window as whole local days, which is what a calendar can express.
	 * `to` is exclusive, so the day it selects is the one containing the
	 * instant just before it — otherwise picking "the 1st to the 1st" would
	 * highlight the 2nd as well.
	 */
	const value = $derived({
		start: calendarDay(range.from),
		end: calendarDay(range.to, -1)
	});

	function calendarDay(instant: string | undefined, offsetMs = 0): DateValue | undefined {
		if (!instant) return undefined;
		const at = new Date(new Date(instant).getTime() + offsetMs);
		if (Number.isNaN(at.getTime())) return undefined;
		return toCalendarDate(fromDate(at, zone));
	}

	/** A picked pair of days becomes the half-open instant window the API takes. */
	function pick(picked: { start?: DateValue; end?: DateValue } | undefined) {
		if (!picked?.start || !picked.end) return;
		const start = (picked.start as CalendarDate).toDate(zone);
		// The end day is included, so the exclusive bound is the midnight
		// after it.
		const end = (picked.end as CalendarDate).add({ days: 1 }).toDate(zone);
		onchange({ from: start.toISOString(), to: end.toISOString() });
		open = false;
	}

	function apply(next: Range) {
		onchange(next);
		open = false;
	}

	let open = $state(false);

	const cell =
		'size-8 rounded-md text-sm data-selected:bg-accent-soft data-selected:text-accent ' +
		'data-disabled:text-subtle data-outside-month:text-subtle data-unavailable:line-through ' +
		'data-selection-start:bg-accent data-selection-start:text-on-accent ' +
		'data-selection-end:bg-accent data-selection-end:text-on-accent ' +
		'hover:bg-raised cursor-pointer transition-colors duration-100';
	const arrow =
		'text-muted hover:bg-raised hover:text-fg inline-flex size-7 cursor-pointer items-center ' +
		'justify-center rounded-md transition-colors duration-100';
</script>

<DateRangePicker.Root
	bind:open
	{value}
	onValueChange={pick}
	weekdayFormat="short"
	numberOfMonths={1}
>
	<DateRangePicker.Trigger>
		{#snippet child({ props })}
			<Button {...props} aria-label="Time range: {label}">
				<CalendarDays class="size-4" />
				<span class="max-w-56 truncate">{label}</span>
			</Button>
		{/snippet}
	</DateRangePicker.Trigger>

	<!-- Portalled for the same reason the filter popover is: the bar it sits in
	     scrolls sideways on a narrow screen, and a calendar clipped by its own
	     toolbar is unusable. -->
	<Portal>
		<DateRangePicker.Content
			sideOffset={6}
			align="start"
			class="border-border bg-canvas shadow-overlay z-50 w-[min(20rem,calc(100vw-1.5rem))] rounded-lg border p-3"
		>
			<div class="flex flex-wrap gap-1.5">
				{#each PRESETS as shortcut (shortcut.key)}
					<Button
						variant={preset === shortcut.key ? 'primary' : 'default'}
						aria-pressed={preset === shortcut.key}
						onclick={() => apply({ from: new Date(Date.now() - shortcut.ms).toISOString() })}
					>
						{shortcut.label}
					</Button>
				{/each}
			</div>

			<DateRangePicker.Calendar class="mt-3">
				{#snippet children({ months, weekdays })}
					<DateRangePicker.Header class="flex items-center justify-between">
						<DateRangePicker.PrevButton class={arrow} aria-label="Previous month">
							‹
						</DateRangePicker.PrevButton>
						<DateRangePicker.Heading class="text-sm font-medium" />
						<DateRangePicker.NextButton class={arrow} aria-label="Next month">
							›
						</DateRangePicker.NextButton>
					</DateRangePicker.Header>
					{#each months as month (month.value.toString())}
						<DateRangePicker.Grid class="mt-2 w-full border-collapse">
							<DateRangePicker.GridHead>
								<DateRangePicker.GridRow class="flex justify-between">
									{#each weekdays as weekday (weekday)}
										<DateRangePicker.HeadCell class="text-subtle size-8 text-xs font-normal">
											{weekday.slice(0, 2)}
										</DateRangePicker.HeadCell>
									{/each}
								</DateRangePicker.GridRow>
							</DateRangePicker.GridHead>
							<DateRangePicker.GridBody>
								{#each month.weeks as week, index (index)}
									<DateRangePicker.GridRow class="flex w-full justify-between">
										{#each week as date (date.toString())}
											<DateRangePicker.Cell {date} month={month.value} class="p-0">
												<DateRangePicker.Day class={cell} />
											</DateRangePicker.Cell>
										{/each}
									</DateRangePicker.GridRow>
								{/each}
							</DateRangePicker.GridBody>
						</DateRangePicker.Grid>
					{/each}
				{/snippet}
			</DateRangePicker.Calendar>

			<div class="border-border mt-3 flex justify-end border-t pt-3">
				<Button onclick={() => apply({})} disabled={!range.from && !range.to}>
					<X class="size-4" />
					Any time
				</Button>
			</div>
		</DateRangePicker.Content>
	</Portal>
</DateRangePicker.Root>
