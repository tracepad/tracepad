<script lang="ts">
	import ChevronsUpDown from '@lucide/svelte/icons/chevrons-up-down';
	import LogOut from '@lucide/svelte/icons/log-out';
	import UserRound from '@lucide/svelte/icons/user-round';
	import { DropdownMenu } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { auth, LOGIN_ROUTE } from '$lib/auth.svelte';
	import { href, project } from '$lib/project.svelte';
	import { end } from '$lib/session';

	// Who is signed in, at the bottom of the sidebar (spec 028 #14). The
	// caption is the role in the project on screen, because that is the one
	// thing about an account that changes what the rest of the window does.

	const role = $derived(project.role);

	async function signOut() {
		await end();
		await goto(LOGIN_ROUTE, { replaceState: true });
	}

	const item =
		'flex w-full cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm ' +
		'text-muted data-highlighted:bg-raised data-highlighted:text-fg pointer-coarse:min-h-11';
</script>

<DropdownMenu.Root>
	<DropdownMenu.Trigger>
		{#snippet child({ props })}
			<!-- An explicit name rather than the words inside it: the caption is
			     the role, and "editor" inside a control's accessible name is a
			     word every "Edit" in the window would also match. -->
			<button
				{...props}
				aria-label="Signed in as {auth.displayName}"
				class="text-fg hover:bg-raised pointer-coarse:min-h-11 flex max-w-40 min-w-0 items-center
					gap-1.5 rounded-md px-2 py-1 transition-colors duration-100 md:w-full md:max-w-none"
			>
				<UserRound class="text-subtle size-4 shrink-0" />
				<!-- On a phone the sidebar is one bar across the top and every
				     row of it is height the listing under it does not get
				     (spec 006 #15), so this is the icon alone there and the
				     name and role on a desktop, where the column has room. -->
				<span class="hidden min-w-0 flex-1 text-left md:block">
					<span class="block truncate text-sm">{auth.displayName}</span>
					{#if role}
						<span class="text-subtle block truncate text-xs capitalize">{role}</span>
					{/if}
				</span>
				<ChevronsUpDown class="text-subtle hidden size-3.5 shrink-0 md:block" />
			</button>
		{/snippet}
	</DropdownMenu.Trigger>
	<DropdownMenu.Portal>
		<DropdownMenu.Content
			side="top"
			align="start"
			sideOffset={4}
			class="border-border bg-canvas shadow-overlay z-50 min-w-44 rounded-md border p-1"
		>
			<p class="text-subtle truncate px-2 py-1 text-xs">{auth.account?.email ?? ''}</p>
			<DropdownMenu.Item>
				{#snippet child({ props })}
					<a href={href('/settings/account')} {...props} class={item}>
						<UserRound class="size-4 shrink-0" />
						Account
					</a>
				{/snippet}
			</DropdownMenu.Item>
			<DropdownMenu.Item onSelect={signOut}>
				{#snippet child({ props })}
					<button {...props} type="button" class={item}>
						<LogOut class="size-4 shrink-0" />
						Sign out
					</button>
				{/snippet}
			</DropdownMenu.Item>
		</DropdownMenu.Content>
	</DropdownMenu.Portal>
</DropdownMenu.Root>
