<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Lock from '@lucide/svelte/icons/lock';
	import Plus from '@lucide/svelte/icons/plus';
	import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
	import Shield from '@lucide/svelte/icons/shield';
	import { admin } from '$lib/admin.svelte';
	import { ApiError, api, type DryRun, type NewKey, type Project } from '$lib/api/client.svelte';
	import { project } from '$lib/project.svelte';
	import { timestamp } from '$lib/format';
	import Button from '../Button.svelte';
	import ConfirmCard from '../ConfirmCard.svelte';
	import SecretDialog from '../SecretDialog.svelte';
	import Card from './Card.svelte';

	// The management plane (spec 007 #3, #4): project lifecycle, and nothing
	// else. Everything above this section runs on the session's project key;
	// everything in it runs on the admin token, which is stored separately and
	// goes only to these endpoints.
	//
	// Lifecycle is what only this token can do (spec 005 #11) — that is the
	// section's reason to exist. Editing another project's retention or keys
	// stays with that project's own key, or with one line of CLI.

	let token = $state('');
	let unlocking = $state(false);
	let unlockFailure = $state<string | null>(null);

	let projects = $state.raw<Project[]>([]);
	let loading = $state(false);
	let failure = $state<string | null>(null);

	let newName = $state('');
	let creating = $state(false);
	let minted = $state.raw<NewKey | null>(null);
	/** The project a deletion is being walked through, if any. */
	let deleting = $state<Project | null>(null);
	/** What just happened, said outside the card that closes on saying it. */
	let notice = $state<string | null>(null);

	$effect(() => {
		if (admin.unlocked) list();
	});

	async function unlock(event: SubmitEvent) {
		event.preventDefault();
		const candidate = token.trim();
		if (!candidate || unlocking) return;
		unlocking = true;
		unlockFailure = null;
		try {
			// Probed with a request only the admin token may make: a project
			// key reaches `GET /api/v1/projects` perfectly well and would
			// otherwise pass for one.
			if (await api.probeAdmin(candidate)) {
				admin.adopt(candidate);
				token = '';
			} else {
				unlockFailure = 'That is not this server’s admin token.';
			}
		} catch (cause) {
			unlockFailure = cause instanceof ApiError ? cause.message : 'Could not check that token.';
		} finally {
			unlocking = false;
		}
	}

	async function list() {
		loading = true;
		failure = null;
		try {
			projects = (await api.listAllProjects()).projects;
		} catch (cause) {
			projects = [];
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the projects.';
		} finally {
			loading = false;
		}
	}

	async function create(event: SubmitEvent) {
		event.preventDefault();
		if (!newName.trim() || creating) return;
		creating = true;
		failure = null;
		try {
			minted = await api.createProject(newName.trim());
			newName = '';
			await list();
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to create the project.';
		} finally {
			creating = false;
		}
	}

	async function restore(target: Project) {
		failure = null;
		try {
			await api.restoreProject(target.id);
			await list();
			notice = `${target.name} is restored.`;
			if (target.id === project.current?.id) await project.refresh();
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to restore the project.';
		}
	}

	async function remove(target: Project, confirm?: string): Promise<DryRun | string> {
		const answer = await api.deleteProject(target.id, confirm);
		if ('dry_run' in answer && answer.dry_run) return answer as DryRun;
		await list();
		notice = `${target.name} is deleted; it can be restored until its purge date.`;
		return notice;
	}

	function lock() {
		admin.clear();
		projects = [];
		deleting = null;
	}
</script>

<Card
	title="Administration"
	description="Project lifecycle — the one thing a project key cannot do. It needs this server's
		TRACEPAD_ADMIN_TOKEN, which is kept apart from your project key and is sent only to these
		endpoints. It reads no traces."
>
	{#if !admin.unlocked}
		<form class="flex flex-wrap items-end gap-2" onsubmit={unlock}>
			<div class="min-w-0 flex-1">
				<label for="admin-token" class="text-muted mb-1 block text-xs font-medium">
					Admin token
				</label>
				<input
					id="admin-token"
					type="password"
					bind:value={token}
					autocomplete="off"
					spellcheck="false"
					autocapitalize="off"
					placeholder="TRACEPAD_ADMIN_TOKEN"
					aria-invalid={unlockFailure ? 'true' : undefined}
					aria-describedby={unlockFailure ? 'admin-token-error' : undefined}
					class="border-border bg-canvas placeholder:text-subtle w-full rounded-md border px-2
						py-1 font-mono text-sm"
				/>
			</div>
			<Button type="submit" variant="primary" disabled={!token.trim()} busy={unlocking}>
				{#if unlocking}
					<LoaderCircle class="size-4 animate-spin" />
				{:else}
					<Shield class="size-4" />
				{/if}
				Unlock
			</Button>
		</form>
		{#if unlockFailure}
			<p id="admin-token-error" role="alert" class="text-danger mt-2 text-sm">{unlockFailure}</p>
		{/if}
	{:else}
		<div class="flex flex-wrap items-center justify-between gap-2">
			<p class="text-ok flex items-center gap-1.5 text-sm">
				<Shield class="size-4" />
				Unlocked on this browser.
			</p>
			<Button onclick={lock}>
				<Lock class="size-4" />
				Lock
			</Button>
		</div>

		{#if failure}
			<p role="alert" class="text-danger mt-3 text-sm">{failure}</p>
		{:else if notice}
			<p role="status" class="text-ok mt-3 text-sm">{notice}</p>
		{/if}

		{#if loading}
			<p class="text-subtle mt-3 flex items-center gap-2 text-sm">
				<LoaderCircle class="size-4 animate-spin" />
				Reading the projects
			</p>
		{:else}
			<div class="border-border mt-3 overflow-x-auto rounded-md border">
				<table class="w-full min-w-2xl border-collapse text-left">
					<thead class="text-subtle text-xs whitespace-nowrap">
						<tr class="border-border border-b">
							<th scope="col" class="px-3 py-1.5 font-medium">Name</th>
							<th scope="col" class="px-3 py-1.5 font-medium">Id</th>
							<th scope="col" class="px-3 py-1.5 font-medium">Retention</th>
							<th scope="col" class="px-3 py-1.5 font-medium">Status</th>
							<th scope="col" class="w-40 px-3 py-1.5 font-medium">Actions</th>
						</tr>
					</thead>
					<tbody>
						{#each projects as row (row.id)}
							<tr class="border-border border-b last:border-b-0">
								<th scope="row" class="px-3 py-1.5 text-left font-normal">{row.name}</th>
								<td class="text-muted px-3 py-1.5 font-mono text-xs">{row.id}</td>
								<td class="text-muted px-3 py-1.5 text-sm">
									{row.retention_days === null
										? 'Keep forever'
										: `${row.retention_days} ${row.retention_days === 1 ? 'day' : 'days'}`}
								</td>
								<td class="px-3 py-1.5 text-sm">
									{#if row.deleted_at}
										<!-- Colour is never the message on its own. -->
										<span class="text-danger">
											Deleted, purged {timestamp(row.purge_at)}
										</span>
									{:else}
										<span class="text-muted">Live</span>
									{/if}
								</td>
								<td class="px-3 py-1.5">
									{#if row.deleted_at}
										<Button onclick={() => restore(row)}>
											<RotateCcw class="size-4" />
											Restore
										</Button>
									{:else}
										<Button
											onclick={() => (
												(notice = null), (deleting = deleting?.id === row.id ? null : row)
											)}
											aria-expanded={deleting?.id === row.id}
										>
											Delete
										</Button>
									{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}

		{#if deleting}
			{@const target = deleting}
			<div class="mt-3">
				<ConfirmCard
					title="Delete {target.name}"
					description="Its keys stop working immediately. The data is restorable for seven days,
						after which the sweeper destroys it."
					echoLabel="project name"
					previewLabel="Show what it holds"
					executeLabel="Delete the project"
					preview={() => remove(target)}
					execute={(confirm) => remove(target, confirm) as Promise<string>}
					ondone={() => (deleting = null)}
				/>
			</div>
		{/if}

		<form class="mt-4 flex flex-wrap items-end gap-2" onsubmit={create}>
			<div class="min-w-0 flex-1">
				<label for="new-project" class="text-muted mb-1 block text-xs font-medium">
					New project
				</label>
				<input
					id="new-project"
					type="text"
					bind:value={newName}
					placeholder="staging"
					autocomplete="off"
					spellcheck="false"
					class="border-border bg-canvas placeholder:text-subtle w-full rounded-md border px-2
						py-1 text-sm"
				/>
			</div>
			<Button type="submit" disabled={!newName.trim()} busy={creating}>
				{#if creating}
					<LoaderCircle class="size-4 animate-spin" />
				{:else}
					<Plus class="size-4" />
				{/if}
				Create
			</Button>
		</form>
		<p class="text-subtle mt-1 text-xs">Its first key pair is shown once, when it is created.</p>
	{/if}
</Card>

<SecretDialog pair={minted} onclose={() => (minted = null)} />
