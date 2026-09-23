import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MediaRef } from '$lib/media';
import MediaStrip from './MediaStrip.svelte';

// The panel's media (spec 041 #10): a thumbnail for an image, a chip that
// downloads anything else, and a muted chip where the project kept no bytes.

const media = vi.fn();
vi.mock('$lib/api/client.svelte', () => ({
	api: { media: (...args: unknown[]) => media(...args) }
}));

const png: MediaRef = { tracepad_media: 'a'.repeat(64), mime_type: 'image/png', size: 20_000 };
const pdf: MediaRef = { tracepad_media: 'b'.repeat(64), mime_type: 'application/pdf', size: 5_000 };
const kept: MediaRef = { tracepad_media: 'c'.repeat(64), mime_type: 'image/jpeg', size: 9_000, stored: false };

beforeEach(() => {
	media.mockReset();
	media.mockImplementation(async () => new Blob(['bytes'], { type: 'image/png' }));
	URL.createObjectURL = vi.fn(() => 'blob:tracepad/1');
	URL.revokeObjectURL = vi.fn();
});
afterEach(() => vi.restoreAllMocks());

describe('the media strip', () => {
	it('draws an image as a thumbnail that opens the full picture', async () => {
		render(MediaStrip, { refs: [png] });
		const open = screen.getByRole('button', { name: 'Open the full image, image/png · 20 KB' });
		expect(media).toHaveBeenCalledWith(png.tracepad_media, expect.any(AbortSignal));
		await waitFor(() => expect(open).toBeEnabled());
		expect(open.querySelector('img')?.getAttribute('src')).toBe('blob:tracepad/1');
		expect(screen.getByText('image/png · 20 KB')).toBeInTheDocument();

		// The tab draws a URL of its own, which outlives the strip's and goes
		// when the tab does.
		vi.mocked(URL.createObjectURL).mockReturnValueOnce('blob:tracepad/tab');
		const view = document.implementation.createHTMLDocument('');
		const listeners: Record<string, () => void> = {};
		const opened = vi.spyOn(window, 'open').mockReturnValue({
			document: view,
			addEventListener: (type: string, listener: () => void) => (listeners[type] = listener)
		} as unknown as Window);
		await userEvent.click(open);
		expect(opened).toHaveBeenCalledWith('', '_blank');
		expect(view.querySelector('img')?.getAttribute('src')).toBe('blob:tracepad/tab');
		expect(URL.revokeObjectURL).not.toHaveBeenCalledWith('blob:tracepad/tab');
		listeners.pagehide?.();
		expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:tracepad/tab');
	});

	it('offers anything else as a download, fetched only when asked', async () => {
		render(MediaStrip, { refs: [pdf] });
		expect(media).not.toHaveBeenCalled();
		const clicked = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
		await userEvent.click(screen.getByRole('button', { name: 'Download application/pdf · 5 KB' }));
		await waitFor(() => expect(clicked).toHaveBeenCalled());
		expect(media).toHaveBeenCalledWith(pdf.tracepad_media);
	});

	it('says a body was not stored, and fetches nothing for it', () => {
		render(MediaStrip, { refs: [kept] });
		expect(screen.getByText('image/jpeg · 9 KB · not stored (project setting)')).toBeInTheDocument();
		expect(screen.queryByRole('button')).not.toBeInTheDocument();
		expect(media).not.toHaveBeenCalled();
	});

	it('drops its URLs with the refs they were made for, and makes none after', async () => {
		const view = render(MediaStrip, { refs: [png] });
		const open = screen.getByRole('button', { name: /Open the full image/ });
		await waitFor(() => expect(open).toBeEnabled());

		// New refs, same picture: the old URL is revoked and no longer drawn
		// while the next fetch is in flight.
		let land: (blob: Blob) => void = () => {};
		media.mockImplementation(() => new Promise<Blob>((resolve) => (land = resolve)));
		await view.rerender({ refs: [{ ...png }] });
		expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:tracepad/1');
		await waitFor(() => expect(open.querySelector('img')).toBeNull());

		// A body that lands after the strip is gone makes no URL.
		view.unmount();
		vi.mocked(URL.createObjectURL).mockClear();
		land(new Blob(['late']));
		await Promise.resolve();
		expect(URL.createObjectURL).not.toHaveBeenCalled();
	});

	it('says so when a picture cannot be loaded', async () => {
		media.mockRejectedValue(new Error('404'));
		render(MediaStrip, { refs: [png] });
		expect(await screen.findByText('could not load')).toBeInTheDocument();
	});
});
