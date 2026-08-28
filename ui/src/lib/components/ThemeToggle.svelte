<script lang="ts">
	import { Monitor, Moon, Sun } from '@lucide/svelte';
	import { theme } from '$lib/theme.svelte';

	// One control, three states (spec 006 #4). A three-way switch as three
	// buttons would spend a row of the sidebar on a setting nobody changes
	// twice; cycling keeps it to one, and the label always names the state it
	// is in rather than the one it will move to.
	const LABELS = {
		system: { icon: Monitor, text: 'Theme: system' },
		light: { icon: Sun, text: 'Theme: light' },
		dark: { icon: Moon, text: 'Theme: dark' }
	};

	const current = $derived(LABELS[theme.value]);
	const Glyph = $derived(current.icon);
</script>

<button
	type="button"
	onclick={() => theme.cycle()}
	title={current.text}
	aria-label={current.text}
	class="text-muted hover:bg-raised hover:text-fg pointer-coarse:size-11 inline-flex size-7
		cursor-pointer items-center justify-center rounded-md transition-colors duration-100"
>
	<Glyph class="size-4" />
</button>
