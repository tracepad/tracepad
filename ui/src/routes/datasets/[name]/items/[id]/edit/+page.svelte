<script lang="ts">
	import { page } from '$app/state';
	import ItemEditor from '$lib/components/evals/ItemEditor.svelte';

	// One case, edited (spec 016 #5). The dataset and the item are both in the
	// path: an existing item belongs where it is, and moving it between
	// datasets is not a write spec 014 has.
	//
	// The item is always edited at the head. An edit *is* a new version at the
	// head (#4), so there is no `?version=` here: the panes are seeded from the
	// item as it is now, and Save writes the next row of it.

	const dataset = $derived(page.params.name ?? '');
	const id = $derived(page.params.id ?? '');
</script>

<ItemEditor {dataset} {id} />
