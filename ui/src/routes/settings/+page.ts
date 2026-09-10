import { redirect } from '@sveltejs/kit';
import type { PageLoad } from './$types';

// `/settings` is the three tabs, and the first of them is the project on
// screen (spec 028, Application contract). Redirecting rather than rendering
// keeps one address per tab, which is what makes a tab a link.
export const load: PageLoad = () => {
	redirect(307, '/settings/project');
};
