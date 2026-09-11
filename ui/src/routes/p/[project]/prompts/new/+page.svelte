<script lang="ts">
	import EditorOnly from '$lib/components/EditorOnly.svelte';
	import PromptEditor from '$lib/components/prompts/PromptEditor.svelte';

	// A new name (spec 021 #4). The editor is the same one that appends a
	// version; what a new name adds is the name and the type, and a name with
	// no history has no labels to offer.
</script>

<EditorOnly what="writing a prompt here is not yours to do">
	<PromptEditor />
</EditorOnly>
