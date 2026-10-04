import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Truncation } from '$lib/api/client.svelte';
import Payload, { isTruncated } from './Payload.svelte';

vi.mock('$lib/api/client.svelte', () => ({
	api: { media: async () => new Blob(['bytes'], { type: 'image/png' }) }
}));

// The viewer consumes the truncation contract of spec 004; it never
// re-implements the budget that produced it.

const preview = '[{"role":"user","content":"the first two hundred characters of it';

const marker: Truncation = {
	truncated: true,
	size: 45_600,
	preview,
	trace_id: 'a'.repeat(32),
	observation_id: 'b'.repeat(16),
	full: '/api/v1/observations/bbbbbbbbbbbbbbbb/io?trace_id=' + 'a'.repeat(32)
};

describe('recognising a truncation marker', () => {
	it('takes only the shape the API actually sends', () => {
		expect(isTruncated(marker)).toBe(true);
		expect(isTruncated({ truncated: true })).toBe(false);
		// A payload that happens to carry the word is still a payload.
		expect(isTruncated({ truncated: 'yes', full: 'x' })).toBe(false);
		expect(isTruncated('a string')).toBe(false);
		expect(isTruncated(null)).toBe(false);
	});
});

describe('a payload the budget could not carry', () => {
	it('shows the preview as a document and offers the whole of it, by size', async () => {
		const onload = vi.fn();
		render(Payload, { label: 'Input', value: marker, loading: false, onload });

		// The preview is a prefix cut on a UTF-8 boundary, so it is text and
		// not a value to parse (spec 015 #3) — and it is the whole preview,
		// which is what the region holds rather than what is drawn in it.
		const region = screen.getByLabelText('Input preview');
		expect(region.closest('.cm-editor')).not.toBeNull();
		expect(region.textContent).toContain('the first two hundred characters');

		const banner = screen.getByRole('button', { name: /showing 65 B of 46 KB/i });
		expect(banner).toHaveAccessibleName(/Load the whole payload/i);

		// And no Copy over a prefix: the banner is how the whole payload is
		// got, and a button offering to copy it that put 65 B of it on the
		// clipboard would be worse than no button at all (spec 015 #3).
		expect(screen.queryByRole('button', { name: /copy/i })).not.toBeInTheDocument();

		await userEvent.setup().click(banner);
		expect(onload).toHaveBeenCalledOnce();
	});

	it('is the banner alone when nothing fit under the share', async () => {
		// spec 004 #25: a marker whose share left no room for a preview.
		const onload = vi.fn();
		render(Payload, {
			label: 'Input',
			value: { ...marker, preview: undefined },
			loading: false,
			onload
		});

		expect(screen.queryByLabelText('Input preview')).not.toBeInTheDocument();
		await userEvent.setup().click(screen.getByRole('button', { name: /load the whole 46 KB/i }));

		expect(onload).toHaveBeenCalledOnce();
	});

	it('swaps in the real value once it has been loaded', () => {
		// The owner re-renders with what `/observations/{id}/io` returned.
		render(Payload, {
			label: 'Input',
			value: { role: 'user', content: 'hello' },
			loading: false,
			onload: vi.fn()
		});

		expect(screen.queryByRole('button', { name: /load the whole/i })).not.toBeInTheDocument();
		expect(screen.getByLabelText('Input').textContent).toContain('"content"');
		// And now that what is on screen *is* the whole payload, Copy is back.
		expect(screen.getByRole('button', { name: /copy the whole input/i })).toBeInTheDocument();
	});

	it('says it is working and stops accepting clicks', () => {
		render(Payload, { label: 'Input', value: marker, loading: true, onload: vi.fn() });

		expect(screen.getByRole('button', { name: /load the whole/i })).toBeDisabled();
	});
});

// Found upgrading a live install: the image was in the stored input, past the
// preview, and the screen read as if it had been dropped. The marker names it
// (spec 004 #39), and the panel shows it.
describe('a payload cut before its media (spec 004 #39)', () => {
	const sha = 'd'.repeat(64);
	beforeEach(() => {
		URL.createObjectURL = vi.fn(() => 'blob:tracepad/1');
		URL.revokeObjectURL = vi.fn();
	});

	it('draws the reference the preview does not reach, and says how many', () => {
		render(Payload, {
			label: 'Input',
			value: {
				...marker,
				media_count: 1,
				media: [{ tracepad_media: sha, mime_type: 'image/png', size: 48_210 }]
			},
			loading: false,
			onload: () => {}
		});

		expect(screen.getByText('image/png · 48 KB')).toBeInTheDocument();
		expect(screen.getByTestId('hidden-media')).toHaveTextContent(
			'1 image or file is referenced past this preview; the whole payload has it.'
		);
	});

	it('counts the ones the marker had no room to spell out', () => {
		render(Payload, {
			label: 'Input',
			value: {
				...marker,
				media_count: 20,
				media: [{ tracepad_media: sha, mime_type: 'image/png', size: 48_210 }]
			},
			loading: false,
			onload: () => {}
		});

		expect(screen.getByTestId('hidden-media')).toHaveTextContent(
			'20 images or files are referenced past this preview — the first 1 shown here'
		);
	});

	it('says nothing when the cut left no media out', () => {
		render(Payload, { label: 'Input', value: marker, loading: false, onload: () => {} });
		expect(screen.queryByTestId('hidden-media')).not.toBeInTheDocument();
	});
});

describe('a trace whose expansion was refused', () => {
	it('offers a load button for the payload instead of a marker', async () => {
		// spec 004 #31: too many payloads for the budget to carry markers, so
		// none were inlined at all and the tree is all that came back.
		const onload = vi.fn();
		render(Payload, { label: 'Output', value: undefined, refused: true, loading: false, onload });

		expect(screen.getByText(/more payloads than the response budget/i)).toBeInTheDocument();
		await userEvent.setup().click(screen.getByRole('button', { name: /load output/i }));

		expect(onload).toHaveBeenCalledOnce();
	});

	it('does not offer to load a payload that is simply absent', () => {
		render(Payload, { label: 'Output', value: undefined, loading: false, onload: vi.fn() });

		expect(screen.queryByRole('button', { name: /load/i })).not.toBeInTheDocument();
	});

	it('stops offering once the fetch has happened and there was nothing', () => {
		// `refused` is a fact about the trace and never changes, so an
		// observation that genuinely carries no metadata would otherwise keep
		// offering to load it — and the click would change nothing, forever.
		render(Payload, {
			label: 'Metadata',
			value: undefined,
			refused: true,
			loaded: true,
			loading: false,
			onload: vi.fn()
		});

		expect(screen.queryByRole('button', { name: /load/i })).not.toBeInTheDocument();
		expect(screen.getByText('—')).toBeInTheDocument();
	});
});

describe('a payload that references media (spec 041 #10)', () => {
	it('draws the media above the JSON, which still shows the reference as data', () => {
		URL.createObjectURL = vi.fn(() => 'blob:tracepad/1');
		URL.revokeObjectURL = vi.fn();
		const ref = { tracepad_media: 'd'.repeat(64), mime_type: 'image/png', size: 20_000 };
		const value = [{ role: 'user', content: [{ type: 'image_url', image_url: { url: ref } }] }];
		render(Payload, { label: 'Input', value, loading: false, onload: vi.fn() });
		expect(screen.getByRole('list', { name: 'Media' })).toBeInTheDocument();
		expect(screen.getByRole('button', { name: /Open the full image, image\/png/ })).toBeInTheDocument();
		expect(screen.getByLabelText('Input').textContent).toContain('tracepad_media');
	});
});
