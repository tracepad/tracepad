<script lang="ts">
	import type { DatasetItem } from '$lib/api/client.svelte';
	import { preview, short } from '$lib/evals';
	import { Fold } from '$lib/fold.svelte';
	import { modified, selecting } from '$lib/peek';
	import Folded from '../Folded.svelte';

	// A dataset's items at a version, in first-appearance order (spec 014
	// #21), mapping 1:1 onto `GET /api/v1/datasets/{name}/items`. The two
	// previews are the first characters of each body as compact JSON: enough
	// to tell the cases apart, and the row's peek shows them whole.

	let {
		rows,
		onopen,
		href,
		selectedID = null
	}: {
		rows: DatasetItem[];
		/** An unmodified left click opens the peek panel (spec 008 #3). */
		onopen: (id: string) => void;
		/**
		 * Where the row's link leads. Caller-supplied because it must carry the
		 * rest of the screen's state — the version in force above all: a
		 * ⌘-click that dropped `?version=` would open the item at the head, or
		 * at nothing when it has since been archived (found in review of this
		 * PR).
		 */
		href: (id: string) => string;
		selectedID?: string | null;
	} = $props();

	/** The row opens the panel; its cells stay selectable text (spec 008 #15). */
	function open(event: MouseEvent, id: string) {
		if (modified(event)) return;
		event.preventDefault();
		if (selecting(event)) return;
		(event.currentTarget as HTMLElement).querySelector('a')?.focus();
		onopen(id);
	}

	const cell = 'truncate px-3 py-1.5 font-mono text-xs';

	// In a box narrower than the table a row is the item and its input; what
	// it expects, its number and its version fold under the input (spec 006
	// #22). The number is the unfolded table's width and its `min-width`.
	const fold = new Fold(672);
	const narrow = $derived(fold.narrow);
</script>

<div bind:contentRect={fold.rect} class="min-h-0 flex-1 overflow-auto">
	<table class="w-full table-fixed border-collapse text-left" style:min-width={fold.min}>
		<thead class="bg-canvas text-subtle sticky top-0 z-10 text-xs whitespace-nowrap">
			<tr class="border-border border-b">
				{#if !narrow}
					<th scope="col" class="w-14 px-3 py-2 text-right font-medium">#</th>
				{/if}
				<th scope="col" class="w-28 px-3 py-2 font-medium">Id</th>
				<th scope="col" class="px-3 py-2 font-medium">Input</th>
				{#if !narrow}
					<th scope="col" class="px-3 py-2 font-medium">Expected</th>
					<th scope="col" class="w-20 px-3 py-2 text-right font-medium">Version</th>
				{/if}
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
					{#if !narrow}
						<td class="text-muted px-3 py-1.5 text-right tabular-nums">{row.seq}</td>
					{/if}
					<td class={cell}>
						<!-- The row's one tabbable thing is a link to this same view
						     with the item open; the editor page of spec 016 #5
						     replaces it as the canonical link when it lands. -->
						<a href={href(row.id)} aria-current={lit ? 'true' : undefined} title={row.id}>
							{short(row.id)}
						</a>
					</td>
					{#if narrow}
						{@const expected = preview(row.expected_output)}
						<td class="px-3 py-1.5 text-xs">
							<div class="text-muted truncate font-mono" title={preview(row.input, 400)}>
								{preview(row.input)}
							</div>
							{#if expected}
								<div class="text-subtle truncate font-mono" title={preview(row.expected_output, 400)}>
									→ {expected}
								</div>
							{/if}
							<div class="text-muted tabular-nums">
								<Folded values={[`#${row.seq}`, `v${row.version}`]} />
							</div>
						</td>
					{:else}
						<td class="text-muted {cell}" title={preview(row.input, 400)}>{preview(row.input)}</td>
						<td class="text-muted {cell}" title={preview(row.expected_output, 400)}>
							{preview(row.expected_output) || '—'}
						</td>
						<td class="text-muted px-3 py-1.5 text-right tabular-nums">v{row.version}</td>
					{/if}
				</tr>
			{/each}
		</tbody>
	</table>
</div>
