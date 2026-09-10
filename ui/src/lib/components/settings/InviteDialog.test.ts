import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import InviteDialog from './InviteDialog.svelte';

// The invitation link, shown once (spec 028 #10, #14). There is no mail here,
// so the link *is* the invitation and the owner carries it — which makes this
// dialog the one place it ever appears, exactly like the key pair of spec 007
// #9.

vi.mock('$app/state', () => ({ page: { url: new URL('http://tracepad.test/settings/server') } }));

const LINK = 'http://tracepad.test/invite#token=deadbeef';

describe('the dialog', () => {
	it('shows the link, its expiry, and says it will not be shown again', () => {
		render(InviteDialog, {
			props: { link: LINK, expires: '2026-09-17T10:00:00Z', onclose: () => {} }
		});

		expect(screen.getByText(LINK)).toBeInTheDocument();
		expect(screen.getByRole('button', { name: 'Copy the invitation link' })).toBeInTheDocument();
		expect(screen.getByText(/only time the link is shown/i)).toBeInTheDocument();
	});

	it('is not on screen at all without a link', () => {
		render(InviteDialog, { props: { link: null, expires: '', onclose: () => {} } });

		expect(screen.queryByRole('dialog')).toBeNull();
	});

	it("passes the server's note through", () => {
		render(InviteDialog, {
			props: {
				link: LINK,
				expires: '2026-09-17T10:00:00Z',
				note: 'the old password works until this link is used',
				onclose: () => {}
			}
		});

		expect(screen.getByText(/old password works until this link is used/)).toBeInTheDocument();
	});
});
