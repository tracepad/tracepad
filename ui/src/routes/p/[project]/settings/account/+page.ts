import { redirect } from '@sveltejs/kit';
import type { PageLoad } from './$types';

// The Account tab is about the person, not the project, and lives bare at
// `/settings/account` (spec 029 #14). A link written under a prefix — a
// bookmark from between #1 and #14, a `next=` — lands there, query and all.
export const load: PageLoad = ({ url }) => {
	redirect(307, `/settings/account${url.search}`);
};
