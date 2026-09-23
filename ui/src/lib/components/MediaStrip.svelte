<script lang="ts">
	import FileDown from '@lucide/svelte/icons/file-down';
	import ImageOff from '@lucide/svelte/icons/image-off';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { api } from '$lib/api/client.svelte';
	import { bytes } from '$lib/format';
	import { fileName, isPicture, type MediaRef } from '$lib/media';

	// The media a payload references (spec 041 #10): an image as a thumbnail
	// that opens the full picture, anything else as a chip that downloads it,
	// and a reference the project's setting kept no bytes for as a muted chip
	// that says so. The JSON view below still shows every reference as data.
	//
	// The bytes are fetched, not linked: a session names its project in a
	// header an `<img src>` cannot send, and a blob drawn in an `<img>` — or
	// saved by a download — is never rendered as a document of this origin,
	// whatever type the client declared.

	let { refs }: { refs: MediaRef[] } = $props();

	let urls = $state<Record<string, string>>({});
	let failed = $state<Record<string, boolean>>({});

	$effect(() => {
		const controller = new AbortController();
		const made: string[] = [];
		for (const ref of refs.filter(isPicture)) {
			const sha = ref.tracepad_media;
			api.media(sha, controller.signal).then(
				(blob) => {
					const url = URL.createObjectURL(blob);
					made.push(url);
					urls[sha] = url;
				},
				(cause) => {
					if (cause?.name !== 'AbortError') failed[sha] = true;
				}
			);
		}
		return () => {
			controller.abort();
			made.forEach((url) => URL.revokeObjectURL(url));
		};
	});

	/** The full picture in a tab of its own, drawn by an `<img>` and nothing else. */
	function open(ref: MediaRef) {
		const url = urls[ref.tracepad_media];
		const view = url ? window.open('', '_blank') : null;
		if (!view) return;
		view.document.title = `${ref.mime_type} · ${bytes(ref.size)}`;
		view.document.body.style.cssText =
			'margin:0;min-height:100vh;display:grid;place-items:center;background:#111';
		const img = view.document.createElement('img');
		img.src = url;
		img.alt = `${ref.mime_type}, ${bytes(ref.size)}`;
		img.style.maxWidth = '100%';
		view.document.body.append(img);
	}

	async function download(ref: MediaRef) {
		try {
			const url = URL.createObjectURL(await api.media(ref.tracepad_media));
			const link = document.createElement('a');
			link.href = url;
			link.download = fileName(ref);
			link.click();
			setTimeout(() => URL.revokeObjectURL(url), 1000);
		} catch {
			failed[ref.tracepad_media] = true;
		}
	}

	const chip =
		'border-border text-muted inline-flex max-w-full items-center gap-1.5 rounded-md border px-2 py-1 text-xs tabular-nums';
</script>

<ul class="mb-2 flex flex-wrap gap-2" aria-label="Media">
	{#each refs as ref (ref.tracepad_media + (ref.stored === false ? '-' : '+'))}
		{@const sha = ref.tracepad_media}
		{@const what = `${ref.mime_type} · ${bytes(ref.size)}`}
		<li class="max-w-full">
			{#if ref.stored === false}
				<span class="{chip} bg-canvas opacity-80" title="The project's media setting is placeholder">
					<ImageOff class="size-3.5 shrink-0" />
					<span class="truncate">{what} · not stored (project setting)</span>
				</span>
			{:else if isPicture(ref)}
				<figure class="w-40">
					<button
						type="button"
						onclick={() => open(ref)}
						disabled={!urls[sha]}
						aria-label="Open the full image, {what}"
						class="border-border bg-raised flex h-28 w-40 cursor-pointer items-center justify-center
							overflow-hidden rounded-md border disabled:cursor-default"
					>
						{#if urls[sha]}
							<img src={urls[sha]} alt="" class="max-h-full max-w-full object-contain" />
						{:else if failed[sha]}
							<span class="text-subtle px-2 text-xs">could not load</span>
						{:else}
							<LoaderCircle class="text-subtle size-4 animate-spin" />
						{/if}
					</button>
					<figcaption class="text-subtle mt-1 truncate text-xs tabular-nums">{what}</figcaption>
				</figure>
			{:else}
				<button
					type="button"
					onclick={() => download(ref)}
					class="{chip} bg-raised hover:text-fg pointer-coarse:min-h-11 cursor-pointer"
					aria-label="Download {what}"
				>
					<FileDown class="size-3.5 shrink-0" />
					<span class="truncate">{what}{failed[sha] ? ' · download failed' : ''}</span>
				</button>
			{/if}
		</li>
	{/each}
</ul>
