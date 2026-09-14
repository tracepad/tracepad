<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Plus from '@lucide/svelte/icons/plus';
	import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
	import SlidersHorizontal from '@lucide/svelte/icons/sliders-horizontal';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { said } from '$lib/accounts';
	import { auth } from '$lib/auth.svelte';
	import { api, type DryRun, type Project } from '$lib/api/client.svelte';
	import { timestamp } from '$lib/format';
	import { switchTarget, under } from '$lib/project.svelte';
	import { refresh } from '$lib/session';
	import Button from '../Button.svelte';
	import ConfirmCard from '../ConfirmCard.svelte';
	import Card from './Card.svelte';
	import NewProjectDialog from './NewProjectDialog.svelte';

	// Project lifecycle (spec 007 #4), which used to be the Administration
	// section behind the admin token and is now an owner's tab (spec 028 #14).
	// The table and its ceremony are exactly what they were: create with the
	// keys-once dialog — the one the switcher opens too (spec 029 #7) —
	// delete behind the echo, restore inside the grace window, soft-deleted
	// rows with their purge dates. A project made is a project opened
	// (spec 029 #15): this tab, under the new id.
	//
	// The table is the way into a project's settings (spec 028 #25): a row
	// shows retention and status, and where they are changed is the Project
	// tab of that project — so the name and the row's first action lead
	// there, a switch of project like the sidebar's (spec 029 #6). Nothing
	// here edits a project; Delete is the exception on the row and looks it.

	let projects = $state.raw<Project[]>([]);
	let loading = $state(true);
	let failure = $state<string | null>(null);

	let creating = $state(false);
	/** The project just made, whose Server tab this one leaves for once the keys are put away. */
	let made = $state.raw<Project | null>(null);
	/** The project a deletion is being walked through, if any. */
	let deleting = $state<Project | null>(null);
	/** What just happened, said outside the card that closes on saying it. */
	let notice = $state<string | null>(null);

	$effect(() => {
		void list();
	});

	// The keys are shown once, and this tab is what the dialog is mounted in:
	// leaving before they are dismissed would take them with it. The same tab
	// of the new project is what a switch to it would land on (spec 029 #6),
	// and the remount that the new id brings (#10) re-reads the table.
	function closed() {
		creating = false;
		if (made) void goto(switchTarget(page.url, made.id));
	}

	/** Where a row leads (spec 028 #25): the Project tab of that project. */
	function settings(row: Project): string {
		return under('/settings/project', row.id);
	}

	async function list() {
		loading = true;
		failure = null;
		try {
			const listed = (await api.listAllProjects()).projects;
			// The table is the server's list and the shell's is `me`, read at
			// sign-in and after this tab's own actions (spec 029 #13). A
			// project made elsewhere — the CLI, another owner's session — is
			// in the first and not the second, and a row that led to it would
			// land on the not-there screen, which is decided at the navigation
			// from the `me` in hand: so `me` is read again, before the rows
			// and their buttons appear, when the table knows a live project
			// the shell does not. A read that fails is not the listing
			// failing; the row still says what exists.
			if (listed.some((row) => !row.deleted_at && !known(row.id))) {
				await refresh().catch(() => {});
			}
			projects = listed;
		} catch (cause) {
			projects = [];
			failure = said(cause, 'Failed to read the projects.');
		} finally {
			loading = false;
		}
	}

	function known(id: string) {
		return auth.projects.some((one) => one.id === id);
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
						<th scope="col" class="w-48 px-3 py-1.5 font-medium">Actions</th>
					</tr>
				</thead>
				<tbody>
					{#each projects as row (row.id)}
						<tr class="border-border border-b last:border-b-0">
							<th scope="row" class="px-3 py-1.5 text-left font-normal">
								{#if row.deleted_at}
									{row.name}
								{:else}
									<a href={settings(row)} class="hover:text-accent">{row.name}</a>
								{/if}
							</th>
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
									<div class="flex items-center gap-1">
										<Button onclick={() => goto(settings(row))}>
											<SlidersHorizontal class="size-4" />
											Settings
										</Button>
										<Button
											variant="ghost"
											onclick={() => (
												(notice = null), (deleting = deleting?.id === row.id ? null : row)
											)}
											aria-expanded={deleting?.id === row.id}
										>
											Delete
										</Button>
									</div>
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

	<div class="mt-4 flex flex-wrap items-center gap-3">
		<Button onclick={() => (creating = true)}>
			<Plus class="size-4" />
			New project
		</Button>
		<p class="text-subtle text-xs">
			A project's name, retention and keys are changed in its settings: the name on a row, or
			<em>Settings</em> beside it.
		</p>
	</div>
</Card>

<NewProjectDialog open={creating} onclose={closed} oncreated={(created) => (made = created)} />
