<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import UserPlus from '@lucide/svelte/icons/user-plus';
	import { reaches, said, standing } from '$lib/accounts';
	import {
		api,
		type AccountDeletion,
		type AccountDetail,
		type DryRun,
		type Invitation,
		type Project
	} from '$lib/api/client.svelte';
	import { timeOrNever } from '$lib/format';
	import { Fold } from '$lib/fold.svelte';
	import Button from '../Button.svelte';
	import ConfirmCard from '../ConfirmCard.svelte';
	import AccountDialog from './AccountDialog.svelte';
	import Card from './Card.svelte';
	import { inset } from './inset';
	import InviteDialog from './InviteDialog.svelte';

	// Who can see what (spec 028 #14). This is the page an owner opens to
	// answer that question, so the projects and the roles are on the row rather
	// than behind a click.
	//
	// An account is created without a password and reached by a link, so
	// "Invite" is what "create" is called here, and the same button under
	// "Edit" is how a lost password is replaced (Decision 10).

	let accounts = $state.raw<AccountDetail[]>([]);
	/** The live projects, which is what a membership can name. */
	let projects = $state.raw<Project[]>([]);
	let loading = $state(true);
	let failure = $state<string | null>(null);
	let notice = $state<string | null>(null);

	/** Open on an account when editing, on null when inviting, closed when undefined. */
	let editing = $state.raw<AccountDetail | null | undefined>(undefined);
	let invitation = $state.raw<Invitation | null>(null);
	/** The account a deletion is being walked through, if any. */
	let deleting = $state.raw<AccountDetail | null>(null);
	/**
	 * The keys that account minted, from the dry run: the deletion leaves
	 * them working, and this is when to decide about rotating them (spec 045
	 * #10).
	 */
	let survivors = $state.raw<AccountDeletion['keys']>([]);

	$effect(() => {
		void list();
	});

	async function list() {
		loading = true;
		try {
			const [people, all] = await Promise.all([api.listAccounts(), api.listAllProjects()]);
			accounts = people.accounts;
			projects = all.projects.filter((one) => !one.deleted_at);
			failure = null;
		} catch (cause) {
			failure = said(cause, 'Failed to read the accounts.');
		} finally {
			loading = false;
		}
	}

	async function remove(target: AccountDetail, confirm?: string): Promise<DryRun | string> {
		const answer = await api.deleteAccount(target.id, confirm);
		if (answer && answer.dry_run) {
			survivors = answer.keys;
			return answer as DryRun;
		}
		await list();
		notice = `${target.email} is deleted.`;
		return notice;
	}

	const cell = `${inset} text-sm`;

	/** Colour is never the message on its own (spec 006 #14): the word is there too. */
	const tone = (where: ReturnType<typeof standing>) =>
		where === 'disabled' ? 'text-danger' : where === 'pending' ? 'text-warn' : 'text-muted';

	// In a box narrower than the table the row is the email and the two
	// buttons, stacked; what the other columns say folds under the email in
	// the same order (spec 006 #18, #22). The number is the unfolded table's width
	// and its `min-width`, which the Server tab's card has on a desktop.
	const fold = new Fold(704);
	const narrow = $derived(fold.narrow);
</script>

<Card
	title="Accounts"
	description="Everybody who can sign in to this server, and what they reach. An owner has every
		project; everybody else has the ones on their row."
>
	{#if failure}
		<p role="alert" class="text-danger mb-3 text-sm">{failure}</p>
	{:else if notice}
		<p role="status" class="text-ok mb-3 text-sm">{notice}</p>
	{/if}

	{#if loading}
		<p class="text-subtle flex items-center gap-2 text-sm">
			<LoaderCircle class="size-4 animate-spin" />
			Reading the accounts
		</p>
	{:else}
		<div bind:contentRect={fold.rect} class="border-border overflow-x-auto rounded-md border">
			<table
				class="w-full border-collapse text-left"
				style:min-width={fold.min}
			>
				<thead class="text-subtle text-xs whitespace-nowrap">
					<tr class="border-border border-b">
						<th scope="col" class="{inset} font-medium">Email</th>
						{#if !narrow}
							<th scope="col" class="{inset} font-medium">Name</th>
							<th scope="col" class="{inset} font-medium">Status</th>
							<th scope="col" class="{inset} font-medium">Last login</th>
							<th scope="col" class="{inset} font-medium">Projects</th>
						{/if}
						<th scope="col" class={[inset, 'font-medium', !narrow && 'w-44']}>
							Actions
						</th>
					</tr>
				</thead>
				<tbody>
					{#each accounts as row (row.id)}
						{@const where = standing(row)}
						<tr class="border-border border-b last:border-b-0">
							{#if narrow}
								<!-- A cell, not the row's header: a header is read out before
								     every cell in the row, and this one holds the whole account.
								     The buttons carry the email in their names instead. -->
								<td class="{inset} wrap-anywhere">
									{row.email}
									<div class="text-muted text-xs">
										{#if row.name}{row.name} ·{/if}
										<span class={tone(where)}>{where}</span>
										· last login <span class="tabular-nums">{timeOrNever(row.last_login_at)}</span>
									</div>
									<!-- Named, because the column heads that said what "never" and
									     "none" are about are not there to say it. -->
									<div class="text-muted text-xs">Projects: {reaches(row)}</div>
								</td>
							{:else}
								<th scope="row" class="{inset} text-left font-normal">{row.email}</th>
								<td class="text-muted {cell}">{row.name || '—'}</td>
								<td class={cell}><span class={tone(where)}>{where}</span></td>
								<td class="text-muted {cell} tabular-nums whitespace-nowrap">
									{timeOrNever(row.last_login_at)}
								</td>
								<td class="text-muted {cell}">{reaches(row)}</td>
							{/if}
							<td class={inset}>
								<div class={['flex gap-1.5', narrow && 'flex-col']}>
									<Button
										aria-label={narrow ? `Edit ${row.email}` : undefined}
										onclick={() => ((notice = null), (editing = row))}
									>
										Edit
									</Button>
									<Button
										aria-label={narrow ? `Delete ${row.email}` : undefined}
										onclick={() => (
											(notice = null),
											(survivors = []),
											(deleting = deleting?.id === row.id ? null : row)
										)}
										aria-expanded={deleting?.id === row.id}
									>
										Delete
									</Button>
								</div>
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
				title="Delete {target.email}"
				description="Their memberships, sessions and any live invitation go with them. Nothing they
					wrote does: their scores keep their name and email. Disabling is the reversible way to
					take access away."
				echoLabel="email"
				previewLabel="Show what would go"
				executeLabel="Delete the account"
				subject={target.id}
				preview={() => remove(target)}
				execute={(confirm) => remove(target, confirm) as Promise<string>}
				ondone={() => (deleting = null)}
			>
				{#if survivors.length > 0}
					<p class="text-warn text-sm">
						{survivors.length === 1 ? 'A key' : `${survivors.length} keys`} they minted will keep working
						until revoked:
					</p>
					<ul class="text-muted mt-1 text-sm">
						{#each survivors as key (key.public_key)}
							<li>
								<code class="font-mono">{key.public_key}</code>
								{key.name ? `(${key.name})` : ''} in {key.project_name}, last used
								{timeOrNever(key.last_used_at)}
							</li>
						{/each}
					</ul>
				{/if}
			</ConfirmCard>
		</div>
	{/if}

	<Button class="mt-3" onclick={() => ((notice = null), (editing = null))}>
		<UserPlus class="size-4" />
		Invite
	</Button>
</Card>

{#if editing !== undefined}
	<AccountDialog
		account={editing}
		{projects}
		oninvited={(fresh) => (invitation = fresh)}
		onsaved={list}
		onclose={() => (editing = undefined)}
	/>
{/if}

<InviteDialog
	link={invitation?.invite_url ?? null}
	expires={invitation?.invite_expires_at ?? ''}
	note={invitation?.note}
	onclose={() => (invitation = null)}
/>
