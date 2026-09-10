<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Plus from '@lucide/svelte/icons/plus';
	import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
	import { said } from '$lib/accounts';
	import { api, type DryRun, type NewKey, type Project } from '$lib/api/client.svelte';
	import { timestamp } from '$lib/format';
	import { refresh } from '$lib/session';
	import Button from '../Button.svelte';
	import ConfirmCard from '../ConfirmCard.svelte';
	import SecretDialog from '../SecretDialog.svelte';
	import Card from './Card.svelte';

	// Project lifecycle (spec 007 #4), which used to be the Administration
	// section behind the admin token and is now an owner's tab (spec 028 #14).
	// The table and its ceremony are exactly what they were: create with the
	// keys-once dialog, delete behind the echo, restore inside the grace
	// window, soft-deleted rows with their purge dates.

	let projects = $state.raw<Project[]>([]);
	let loading = $state(true);
	let failure = $state<string | null>(null);

	let newName = $state('');
	let creating = $state(false);
	let minted = $state.raw<NewKey | null>(null);
	/** The project a deletion is being walked through, if any. */
	let deleting = $state<Project | null>(null);
	/** What just happened, said outside the card that closes on saying it. */
	let notice = $state<string | null>(null);

	$effect(() => {
		void list();
	});

	async function list() {
		loading = true;
		failure = null;
		try {
			projects = (await api.listAllProjects()).projects;
		} catch (cause) {
			projects = [];
			failure = said(cause, 'Failed to read the projects.');
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
			// A new project is one an owner can reach, so the shell's list of
			// them has just changed (Decision 15).
			await refresh();
		} catch (cause) {
			failure = said(cause, 'Failed to create the project.');
		} finally {
			creating = false;
		}
	}

	async function restore(target: Project) {
		failure = null;
		try {
			await api.restoreProject(target.id);
			await list();
			await refresh();
			notice = `${target.name} is restored.`;
		} catch (cause) {
			failure = said(cause, 'Failed to restore the project.');
		}
	}

	async function remove(target: Project, confirm?: string): Promise<DryRun | string> {
		const answer = await api.deleteProject(target.id, confirm);
		if ('dry_run' in answer && answer.dry_run) return answer as DryRun;
		await list();
		await refresh();
		notice = `${target.name} is deleted; it can be restored until its purge date.`;
		return notice;
	}
</script>

<Card
	title="Projects"
	description="Every project on this server. Creating, renaming, deleting and restoring one is an
		owner's: a project's name is the echo every destructive confirmation is typed against."
>
	{#if failure}
		<p role="alert" class="text-danger mb-3 text-sm">{failure}</p>
	{:else if notice}
		<p role="status" class="text-ok mb-3 text-sm">{notice}</p>
	{/if}

	{#if loading}
		<p class="text-subtle flex items-center gap-2 text-sm">
			<LoaderCircle class="size-4 animate-spin" />
			Reading the projects
		</p>
	{:else}
		<div class="border-border overflow-x-auto rounded-md border">
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
									<span class="text-danger">Deleted, purged {timestamp(row.purge_at)}</span>
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
				subject={target.id}
				preview={() => remove(target)}
				execute={(confirm) => remove(target, confirm) as Promise<string>}
				ondone={() => (deleting = null)}
			/>
		</div>
	{/if}

	<form class="mt-4 flex flex-wrap items-end gap-2" onsubmit={create}>
		<div class="min-w-0 flex-1">
			<label for="new-project" class="text-muted mb-1 block text-xs font-medium">New project</label>
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
</Card>

<SecretDialog pair={minted} onclose={() => (minted = null)} />
