<script lang="ts">
	import Eye from '@lucide/svelte/icons/eye';
	import EyeOff from '@lucide/svelte/icons/eye-off';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError, api } from '$lib/api/client.svelte';
	import { auth } from '$lib/auth.svelte';
	import Button from '$lib/components/Button.svelte';

	// The app always authenticates (spec 006 #8). This is the only screen that
	// works without a credential, and the only one that hands one out.

	let value = $state('');
	let reveal = $state(false);
	let busy = $state(false);
	let error = $state<string | null>(null);

	// Coming back to where the guard interrupted, which is what makes a deep
	// link survive a sign-in. Only a path of this app: an absolute URL here
	// would be an open redirect handed to whoever wrote the link.
	const next = $derived.by(() => {
		const asked = page.url.searchParams.get('next');
		return asked && asked.startsWith('/') && !asked.startsWith('//') ? asked : '/traces';
	});

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		const key = value.trim();
		if (!key || busy) return;
		busy = true;
		error = null;
		try {
			// Validated with the request the app actually needs, so a
			// credential that cannot read the screens cannot get in (#13).
			const verdict = await api.probe(key);
			if (verdict === 'project-key') {
				auth.adopt(key);
				await goto(next, { replaceState: true });
				return;
			}
			error =
				verdict === 'admin-token'
					? 'That is the admin token. The interface reads traces with a project key; ' +
						'the admin token manages projects and keys.'
					: 'The server did not accept that key.';
		} catch (cause) {
			error =
				cause instanceof ApiError ? cause.message : 'Something went wrong signing in.';
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>Sign in · Tracepad</title></svelte:head>

<div class="flex min-h-dvh items-center justify-center p-6">
	<form class="w-full max-w-sm" onsubmit={submit}>
		<h1 class="text-xl font-semibold tracking-tight">Tracepad</h1>
		<p class="text-muted mt-1">Paste a project key to read this server's traces.</p>

		<label for="key" class="mt-6 mb-1.5 block font-medium">Project key</label>
		<div class="flex gap-1.5">
			<input
				id="key"
				name="key"
				type={reveal ? 'text' : 'password'}
				bind:value
				autocomplete="off"
				spellcheck="false"
				autocapitalize="off"
				placeholder="tp-sk-…"
				aria-describedby={error ? 'key-error' : 'key-hint'}
				aria-invalid={error ? 'true' : undefined}
				class="border-border bg-canvas placeholder:text-subtle min-w-0 flex-1 rounded-md border
					px-2.5 py-1.5 font-mono text-sm"
			/>
			<button
				type="button"
				onclick={() => (reveal = !reveal)}
				aria-label={reveal ? 'Hide the key' : 'Show the key'}
				class="border-border bg-surface text-muted hover:bg-raised hover:text-fg
					pointer-coarse:size-11 inline-flex size-8 shrink-0 cursor-pointer items-center
					justify-center rounded-md border transition-colors duration-100"
			>
				{#if reveal}<EyeOff class="size-4" />{:else}<Eye class="size-4" />{/if}
			</button>
		</div>

		{#if error}
			<p id="key-error" role="alert" class="text-danger mt-2">{error}</p>
		{:else}
			<p id="key-hint" class="text-subtle mt-2 text-sm">
				The server prints one on first run, next to the connection strings.
			</p>
		{/if}

		<Button type="submit" variant="primary" busy={busy} disabled={!value.trim()} class="mt-4 w-full justify-center">
			{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
			Sign in
		</Button>
	</form>
</div>
