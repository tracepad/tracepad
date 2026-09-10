<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import EditorOnly from '$lib/components/EditorOnly.svelte';
	import ItemEditor from '$lib/components/evals/ItemEditor.svelte';

	// A new case (spec 016 #5, #21). The dataset is in the query rather than in
	// the path because this page is where it is chosen: *Add to dataset* (#8)
	// arrives from an observation with no dataset in mind, and the select on
	// the page is the control that sets it. `?trace=`/`?obs=` name the
	// observation the case is being cut from.

	const dataset = $derived(page.url.searchParams.get('dataset') ?? '');
	const trace = $derived(page.url.searchParams.get('trace'));
	const obs = $derived(page.url.searchParams.get('obs'));

	/** Choosing a dataset replaces the entry: it is the same unfinished case. */
	function pick(name: string) {
		const search = new URLSearchParams(page.url.searchParams);
		if (name) search.set('dataset', name);
		else search.delete('dataset');
		const query = search.toString();
		goto(`${page.url.pathname}${query ? `?${query}` : ''}`, {
			replaceState: true,
			keepFocus: true,
			noScroll: true
		});
	}
</script>

<EditorOnly what="writing a dataset case is not yours to do">
	<ItemEditor {dataset} {trace} {obs} onpick={pick} />
</EditorOnly>
