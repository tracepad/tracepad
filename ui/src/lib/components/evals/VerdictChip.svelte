<script lang="ts">
	// One of the four words a comparison can say about a case (spec 014 #16),
	// coloured but never only coloured: the word is the message.
	let { verdict }: { verdict: 'improved' | 'regressed' | 'changed' | 'same' } = $props();

	const tone = $derived(
		verdict === 'improved'
			? 'text-ok'
			: verdict === 'regressed'
				? 'text-danger bg-danger-soft'
				: verdict === 'changed'
					? 'text-warn'
					: 'text-subtle'
	);
</script>

<span class={['rounded px-1.5 py-0.5 text-xs font-medium', tone]}>{verdict}</span>
