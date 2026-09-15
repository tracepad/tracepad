import { redirect } from '@sveltejs/kit';
import { bareTarget } from '$lib/project.svelte';
import type { PageLoad } from './$types';

// `/p` with no project is where an account that reaches none is sent
// (spec 029 #4). For everybody else it is not a screen: the remembered
// project's dashboard is (spec 034 #1), and a bare `/p` in a bookmark lands
// there.
export const load: PageLoad = async ({ parent }) => {
	await parent();
	const target = bareTarget('/dashboard');
	if (target !== '/p') redirect(307, target);
};
