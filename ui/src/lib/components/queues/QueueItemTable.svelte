<script lang="ts">
	import ListX from '@lucide/svelte/icons/list-x';
	import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
	import { MediaQuery } from 'svelte/reactivity';
	import type { AnnotationItem } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import { timestamp } from '$lib/format';
	import { modified, selecting } from '$lib/peek';
	import { folded, PHONE } from '$lib/phone';
	import { project } from '$lib/project.svelte';

	// A queue's items in `seq` order (spec 024 #11): what is done, by whom,
	// what was skipped and why. The row opens the trace it points at, because
	// that is what the item *is* — a pointer — and the two verbs on it are the
	// manager's: put it back in the queue, or take it out.

	let {
		rows,
		onopen,
		href,
		selectedID = null,
		onreopen,
		onremove,
		busyID = null
	}: {
		rows: AnnotationItem[];
		/** An unmodified left click opens the peek panel (spec 008 #3). */
		onopen: (id: string) => void;
		/** Where the row's link leads; caller-supplied so it carries the filters. */
		href: (id: string) => string;
		selectedID?: string | null;
		onreopen: (item: AnnotationItem) => void;
		onremove: (item: AnnotationItem) => void;
		/** The row a write is in flight for, so its buttons say so. */
		busyID?: string | null;
	} = $props();

	/** The row opens the panel; its cells stay selectable text (spec 008 #15). */
	function open(event: MouseEvent, id: string) {
		if (modified(event)) return;
		event.preventDefault();
		if (selecting(event)) return;
		(event.currentTarget as HTMLElement).querySelector('a')?.focus();
		onopen(id);
	}

	const chip = (status: string) =>
		({
			completed: 'text-ok bg-ok-soft',
			skipped: 'text-warn bg-raised',
			pending: 'text-muted bg-raised'
		})[status] ?? 'text-muted bg-raised';

	const cell = 'truncate px-3 py-1.5';

	// On a phone the row is the target, its status and the two verbs, which
	// keep their names for a screen reader and lose them on the screen; the
	// number, who, when and why fold under the target (spec 006 #18).
	const phone = new MediaQuery(PHONE);
</script>

<div class="min-h-0 flex-1 overflow-auto">
	<table class={['w-full table-fixed border-collapse text-left', !phone.current && 'min-w-3xl']}>
		<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
			<tr class="border-border border-b">
				{#if !phone.current}
					<th scope="col" class="w-14 px-3 py-2 text-right font-medium">#</th>
				{/if}
				<th scope="col" class="px-3 py-2 font-medium">Target</th>
				<th scope="col" class="w-28 px-3 py-2 font-medium">Status</th>
				{#if !phone.current}
					<th scope="col" class="w-32 px-3 py-2 font-medium">By</th>
					<th scope="col" class="w-44 px-3 py-2 font-medium">When</th>
					<th scope="col" class="px-3 py-2 font-medium">Skip reason</th>
				{/if}
				<th scope="col" class={['px-3 py-2', phone.current ? 'w-34' : 'w-40']}>
					<span class="sr-only">Actions</span>
				</th>
			</tr>
		</thead>
		<tbody>
			{#each rows as row (row.id)}
				{@const lit = row.id === selectedID}
				<!-- svelte-ignore a11y_click_events_have_key_events -->
				<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
				<tr
					onclick={(event) => open(event, row.id)}
					class={[
						'border-border hover:bg-raised cursor-pointer border-b transition-colors duration-100',
						lit && 'bg-accent-soft'
					]}
				>
					{#if !phone.current}
						<td class="text-muted px-3 py-1.5 text-right tabular-nums">{row.seq}</td>
					{/if}
					<td class="{cell} font-mono text-xs">
						<a href={href(row.id)} aria-current={lit ? 'true' : undefined} title={row.trace_id}>
							{row.trace_id.slice(0, 12)}…{#if row.observation_id}<span class="text-subtle"
									>/{row.observation_id.slice(0, 8)}…</span
								>{/if}
						</a>
						{#if phone.current}
							<div class="text-muted truncate font-sans">
								{folded([
									`#${row.seq}`,
									row.completed_by,
									row.completed_at && timestamp(row.completed_at)
								])}
							</div>
							<!-- The reason is the why, so it gets a line of its own rather
							     than the end of one that is cut first. -->
							{#if row.skip_reason}
								<div
									class="text-muted line-clamp-2 font-sans whitespace-normal"
									title={row.skip_reason}
								>
									{row.skip_reason}
								</div>
							{/if}
						{/if}
					</td>
					<td class="px-3 py-1.5">
						<span class="rounded-md px-1.5 py-0.5 text-xs {chip(row.status)}">{row.status}</span>
					</td>
					{#if !phone.current}
						<td class="text-muted {cell}">{row.completed_by ?? '—'}</td>
						<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
							{row.completed_at ? timestamp(row.completed_at) : '—'}
						</td>
						<td class="text-muted {cell}" title={row.skip_reason ?? undefined}>
							{row.skip_reason ?? '—'}
						</td>
					{/if}
					<td class="px-3 py-1.5">
						<!-- The verbs stop the click here: the row opens the trace,
						     and a Remove that also opened a panel over the item it
						     just deleted would be two answers to one press. -->
						<div class="flex justify-end gap-1.5">
							{#if row.status !== 'pending'}
								<Button
									variant="ghost"
									busy={busyID === row.id}
									aria-label={phone.current ? 'Reopen' : undefined}
									title={phone.current ? 'Reopen' : undefined}
									onclick={(event) => (event.stopPropagation(), onreopen(row))}
								>
									{#if phone.current}<RotateCcw class="size-4" />{:else}Reopen{/if}
								</Button>
							{/if}
							<!-- Working an item is a viewer's job and taking it off
							     the list is not: removing one is queue management,
							     which is an editor's (spec 028 #15). -->
							{#if project.editor}
								<Button
									variant="ghost"
									busy={busyID === row.id}
									aria-label={phone.current ? 'Remove' : undefined}
									title={phone.current ? 'Remove' : undefined}
									onclick={(event) => (event.stopPropagation(), onremove(row))}
								>
									{#if phone.current}<ListX class="size-4" />{:else}Remove{/if}
								</Button>
							{/if}
						</div>
					</td>
				</tr>
			{/each}
		</tbody>
	</table>
</div>
