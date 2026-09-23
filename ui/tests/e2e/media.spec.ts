import { createHash } from 'node:crypto';
import { deflateSync } from 'node:zlib';
import { expect, test, type Page } from '@playwright/test';
import { createProject, signIn as enter, state } from './harness';

// Media in a trace (spec 041, Testing — UI), against the real binary: ingest
// takes a picture out of a generation's input, and the observation's panel
// draws it as a thumbnail that opens the full image; a PDF is a chip that
// downloads; a reference the project kept no bytes for says so.
//
// Its own project, because it writes media that the other suites' counts do
// not expect.

const TRACE = 'ee' + '0'.repeat(29) + '1';
const SPAN = 'ee' + '0'.repeat(13) + '1';

let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('media'));

/** A real PNG of noise — incompressible, so well past the 4 KiB floor. */
function noisePNG(side: number): Buffer {
	const crcTable = Array.from({ length: 256 }, (_, n) => {
		let c = n;
		for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
		return c >>> 0;
	});
	const crc = (bytes: Buffer) => {
		let c = 0xffffffff;
		for (const b of bytes) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8);
		return (c ^ 0xffffffff) >>> 0;
	};
	const chunk = (type: string, data: Buffer) => {
		const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
		const out = Buffer.alloc(12 + data.length);
		out.writeUInt32BE(data.length, 0);
		body.copy(out, 4);
		out.writeUInt32BE(crc(body), 8 + data.length);
		return out;
	};
	const header = Buffer.alloc(13);
	header.writeUInt32BE(side, 0);
	header.writeUInt32BE(side, 4);
	header.set([8, 2, 0, 0, 0], 8);
	const rows = Buffer.alloc(side * (1 + side * 3));
	let seed = 7;
	for (let i = 0; i < rows.length; i++) {
		seed = (seed * 1103515245 + 12345) >>> 0;
		rows[i] = i % (1 + side * 3) === 0 ? 0 : seed >>> 24;
	}
	return Buffer.concat([
		Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
		chunk('IHDR', header),
		chunk('IDAT', deflateSync(rows)),
		chunk('IEND', Buffer.alloc(0))
	]);
}

const picture = noisePNG(48);
const pdf = Buffer.from('%PDF-1.4\n' + 'x'.repeat(6000));

let seeded: Promise<void> | null = null;
function seed(): Promise<void> {
	seeded ??= (async () => {
		const { baseURL } = state();
		const { key } = await project();
		const input = [
			{
				role: 'user',
				content: [
					{ type: 'text', text: 'what is in this picture, and in the attached report' },
					{ type: 'image_url', image_url: { url: `data:image/png;base64,${picture.toString('base64')}` } },
					{ type: 'file', file: { file_data: `data:application/pdf;base64,${pdf.toString('base64')}` } },
					// What a project under the placeholder setting leaves.
					{ tracepad_media: 'f'.repeat(64), mime_type: 'image/jpeg', size: 9000, stored: false }
				]
			}
		];
		const start = Date.parse('2026-09-20T10:00:00Z') * 1_000_000;
		const response = await fetch(`${baseURL}/v1/traces`, {
			method: 'POST',
			headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
			body: JSON.stringify({
				resourceSpans: [
					{
						resource: { attributes: [] },
						scopeSpans: [
							{
								spans: [
									{
										traceId: TRACE,
										spanId: SPAN,
										name: 'describe-picture',
										startTimeUnixNano: String(start),
										endTimeUnixNano: String(start + 900_000_000),
										attributes: [
											{ key: 'langfuse.observation.type', value: { stringValue: 'generation' } },
											{ key: 'langfuse.observation.input', value: { stringValue: JSON.stringify(input) } }
										]
									}
								]
							}
						]
					}
				]
			})
		});
		if (!response.ok) throw new Error(`POST /v1/traces: ${response.status}`);
	})();
	return seeded;
}

async function openObservation(page: Page) {
	await enter(page, (await project()).account);
	await page.goto(`/traces/${TRACE}?obs=${SPAN}`);
	return page.getByRole('list', { name: 'Media' }).first();
}

test.beforeEach(async () => {
	await seed();
});

test('a picture in the input is a thumbnail that opens the full image', async ({ page }) => {
	const strip = await openObservation(page);
	const thumbnail = strip.getByRole('button', { name: /Open the full image, image\/png/ });
	await expect(thumbnail).toBeEnabled();
	const img = thumbnail.locator('img');
	await expect(img).toBeVisible();
	expect(await img.evaluate((node: HTMLImageElement) => node.naturalWidth)).toBe(48);

	const [popup] = await Promise.all([page.waitForEvent('popup'), thumbnail.click()]);
	const full = popup.locator('img');
	await expect(full).toBeVisible();
	expect(await full.evaluate((node: HTMLImageElement) => node.naturalWidth)).toBe(48);

	// The JSON still shows the reference as data, and never the base64.
	const sha = createHash('sha256').update(picture).digest('hex');
	await expect(page.getByText(sha).first()).toBeAttached();
	await expect(page.getByText(picture.toString('base64').slice(0, 40))).toHaveCount(0);
});

test('a file is a chip that downloads, and a body not kept says so', async ({ page }) => {
	const strip = await openObservation(page);
	const chip = strip.getByRole('button', { name: /Download application\/pdf/ });
	await expect(chip).toBeVisible();
	const [download] = await Promise.all([page.waitForEvent('download'), chip.click()]);
	expect(download.suggestedFilename()).toMatch(/^[0-9a-f]{12}\.pdf$/);

	await expect(strip.getByText('not stored (project setting)')).toBeVisible();
});
