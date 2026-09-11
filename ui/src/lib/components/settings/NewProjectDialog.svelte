<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { Dialog } from 'bits-ui';
	import { said } from '$lib/accounts';
	import { api, type NewKey, type Project } from '$lib/api/client.svelte';
	import { refresh } from '$lib/session';
	import Button from '../Button.svelte';
	import SecretDialog from '../SecretDialog.svelte';

	// A project is a name; its first key pair arrives with it and is shown
	// once (spec 007 #3). One component for the two places an owner can ask
	// for one — the Server tab's table and the switcher (spec 029 #7) — so
	// that the keys-once rule has one implementation. `me` is read again
	// before the caller hears of it: a new project is one the owner can
	// reach, and the shell's list of them has just changed (spec 028 #15).

	let {
		open = false,
		onclose,
		oncreated
	}: { open?: boolean; onclose: () => void; oncreated?: (project: Project) => void } = $props();

	let name = $state('');
	let busy = $state(false);
	let failure = $state<string | null>(null);
	let minted = $state.raw<NewKey | null>(null);
	/** A project made, whose `me` could not be read yet: the caller has not heard of it. */
	let unannounced = $state.raw<Project | null>(null);

	// Opened again is a fresh form; nothing carries over from the last one.
	$effect(() => {
		if (open) {
			name = '';
			failure = null;
		}
	});

	async function create(event: SubmitEvent) {
		event.preventDefault();
		if (!name.trim() || busy) return;
		busy = true;
		failure = null;
		let created: Project & NewKey;
		try {
			created = await api.createProject(name.trim());
		} catch (cause) {
			failure = said(cause, 'Failed to create the project.');
			busy = false;
			return;
		}
		// The keys go on screen before anything else can fail: the server says
		// the secret in this answer and never again, so a `me` that cannot be
		// read a moment later is no reason to lose them.
		minted = created;
		await announce(created);
		busy = false;
	}

	/**
	 * `me` again, then the caller: what it does with the project — lists it,
	 * goes to it — needs the shell to reach it first. A read that fails is
	 * tried once more when the keys are put away, and given up after that: the
	 * project exists, and the next reload of the shell reads it.
	 */
	async function announce(created: Project) {
		try {
			await refresh();
		} catch {
			unannounced = created;
			return;
		}
		unannounced = null;
		oncreated?.(created);
	}

	async function done() {
		minted = null;
		if (unannounced) await announce(unannounced);
		unannounced = null;
		onclose();
	}
</script>

<!-- The form closes the moment the keys open: two dialogs stacked would be
     two things to dismiss, and the keys are the one that matters. It stays
     shut while a project made is still being announced: a form with the
     same name in it and Create enabled would mint that project twice. -->
<Dialog.Root
	open={open && minted === null && unannounced === null}
	onOpenChange={(next) => !next && !busy && onclose()}
>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="border-border bg-canvas shadow-overlay fixed top-1/2 left-1/2 z-50 max-h-[90dvh]
				w-[min(28rem,calc(100vw-1.5rem))] -translate-x-1/2 -translate-y-1/2 overflow-y-auto
				rounded-lg border p-4"
		>
			<form onsubmit={create}>
				<Dialog.Title class="text-lg font-semibold">New project</Dialog.Title>
				<Dialog.Description class="text-muted mt-1 text-sm">
					Its first key pair is shown once, when it is created.
				</Dialog.Description>

				<label for="new-project-name" class="mt-3 mb-1 block text-xs font-medium">Name</label>
				<input
					id="new-project-name"
					name="name"
					type="text"
					bind:value={name}
					placeholder="staging"
					autocomplete="off"
					spellcheck="false"
					class="border-border bg-canvas text-fg placeholder:text-subtle w-full rounded-md border
						px-2 py-1 text-sm"
				/>

				{#if failure}
					<p role="alert" class="text-danger mt-3 flex items-start gap-2 text-sm">
						<TriangleAlert class="mt-0.5 size-4 shrink-0" />
						{failure}
					</p>
				{/if}

				<div class="mt-4 flex justify-end gap-1.5">
					<Dialog.Close>
						{#snippet child({ props })}
							<Button {...props} disabled={busy}>Cancel</Button>
						{/snippet}
					</Dialog.Close>
					<Button type="submit" variant="primary" disabled={!name.trim()} {busy}>
						{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
						Create
					</Button>
				</div>
			</form>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>

<SecretDialog pair={minted} onclose={done} />
