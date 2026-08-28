import { redirect } from '@sveltejs/kit';
import type { PageLoad } from './$types';

// The root is not a screen: traces are what the product is for, and the
// pre-authed URL the server prints lands here.
export const load: PageLoad = () => {
	redirect(307, '/traces');
};
