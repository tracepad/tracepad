<script lang="ts">
	import { page } from '$app/state';
	import { api } from '$lib/api/client.svelte';
	import PromptEditor from '$lib/components/prompts/PromptEditor.svelte';

	// A new version of an existing name (spec 021 #4), prefilled from `?from=V`
	// — the version on screen when *New version* was pressed — or the latest.
	//
	// The name's existing labels are fetched here rather than in the editor:
	// they are a property of the name, and the editor that creates one has none
	// to ask for.

	const name = $derived(page.params.name ?? '');
	const raw = $derived(Number(page.url.searchParams.get('from')));
	const from = $derived(Number.isInteger(raw) && raw >= 1 ? raw : null);

	let known = $state.raw<string[]>([]);

	$effect(() => {
		const controller = new AbortController();
		api
			.listPromptVersions(name, { limit: 1 }, controller.signal)
			.then((answer) => {
				if (!controller.signal.aborted) known = Object.keys(answer.labels);
			})
			.catch(() => {
				// The datalist stays empty; a label is free text anyway, and
				// the editor is unaffected.
			});
		return () => controller.abort();
	});
</script>

<PromptEditor {name} {from} {known} />
