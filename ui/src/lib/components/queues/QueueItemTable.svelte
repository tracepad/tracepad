<script lang="ts">
	import type { AnnotationItem } from '$lib/api/client.svelte';
	import Button from '$lib/components/Button.svelte';
	import { timestamp } from '$lib/format';
	import { modified, selecting } from '$lib/peek';

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
</script>

<div class="min-h-0 flex-1 overflow-auto">
	<table class="w-full min-w-3xl table-fixed border-collapse text-left">
		<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
			<tr class="border-border border-b">
				<th scope="col" class="w-14 px-3 py-2 text-right font-medium">#</th>
				<th scope="col" class="px-3 py-2 font-medium">Target</th>
				<th scope="col" class="w-28 px-3 py-2 font-medium">Status</th>
				<th scope="col" class="w-32 px-3 py-2 font-medium">By</th>
				<th scope="col" class="w-44 px-3 py-2 font-medium">When</th>
				<th scope="col" class="px-3 py-2 font-medium">Skip reason</th>
				<th scope="col" class="w-40 px-3 py-2"><span class="sr-only">Actions</span></th>
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
					<td class="text-muted px-3 py-1.5 text-right tabular-nums">{row.seq}</td>
					<td class="{cell} font-mono text-xs">
						<a href={href(row.id)} aria-current={lit ? 'true' : undefined} title={row.trace_id}>
							{row.trace_id.slice(0, 12)}…{#if row.observation_id}<span class="text-subtle"
									>/{row.observation_id.slice(0, 8)}…</span
								>{/if}
						</a>
					</td>
					<td class="px-3 py-1.5">
						<span class="rounded-md px-1.5 py-0.5 text-xs {chip(row.status)}">{row.status}</span>
					</td>
					<td class="text-muted {cell}">{row.completed_by ?? '—'}</td>
					<td class="text-muted px-3 py-1.5 font-mono text-xs whitespace-nowrap tabular-nums">
						{row.completed_at ? timestamp(row.completed_at) : '—'}
					</td>
					<td class="text-muted {cell}" title={row.skip_reason ?? undefined}>
						{row.skip_reason ?? '—'}
					</td>
					<td class="px-3 py-1.5">
						<!-- The verbs stop the click here: the row opens the trace,
						     and a Remove that also opened a panel over the item it
						     just deleted would be two answers to one press. -->
						<div class="flex justify-end gap-1.5">
							{#if row.status !== 'pending'}
								<Button
									variant="ghost"
									busy={busyID === row.id}
									onclick={(event) => (event.stopPropagation(), onreopen(row))}
								>
									Reopen
								</Button>
							{/if}
							<Button
								variant="ghost"
								busy={busyID === row.id}
								onclick={(event) => (event.stopPropagation(), onremove(row))}
							>
								Remove
							</Button>
						</div>
					</td>
				</tr>
			{/each}
		</tbody>
	</table>
</div>
