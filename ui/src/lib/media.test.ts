import { describe, expect, it } from 'vitest';
import { fileName, isMediaRef, isPicture, mediaRefs, type MediaRef } from './media';

const sha = (c: string) => c.repeat(64);
const png: MediaRef = { tracepad_media: sha('a'), mime_type: 'image/png', size: 20_000 };
const pdf: MediaRef = { tracepad_media: sha('b'), mime_type: 'application/pdf', size: 5_000 };
const kept: MediaRef = { ...png, tracepad_media: sha('c'), stored: false };

describe('media references (spec 041 #4)', () => {
	it('takes only the shape ingest leaves', () => {
		expect(isMediaRef(png)).toBe(true);
		expect(isMediaRef({ tracepad_media: 'not a hash', mime_type: 'image/png' })).toBe(false);
		expect(isMediaRef({ tracepad_media: sha('a') })).toBe(false);
		expect(isMediaRef('data:image/png;base64,AAAA')).toBe(false);
	});

	it('finds every reference in a payload once, in order', () => {
		const value = [
			// In an object shape the reference sits in the base64's slot
			// (Decision 19).
			{
				role: 'user',
				content: [
					{ type: 'image', source: { type: 'base64', media_type: 'image/png', data: png } },
					{ type: 'text', text: 'hi' }
				]
			},
			{ role: 'user', content: [{ type: 'image_url', image_url: { url: png } }, pdf, kept] }
		];
		expect(mediaRefs(value)).toEqual([png, pdf, kept]);
		expect(mediaRefs('a string')).toEqual([]);
		expect(mediaRefs(null)).toEqual([]);
	});

	it('draws only an image whose bytes were kept', () => {
		expect(isPicture(png)).toBe(true);
		expect(isPicture(pdf)).toBe(false);
		expect(isPicture(kept)).toBe(false);
	});

	it('names a download by the hash and the type', () => {
		expect(fileName(pdf)).toBe('bbbbbbbbbbbb.pdf');
		expect(fileName({ ...png, mime_type: 'image/svg+xml' })).toBe('aaaaaaaaaaaa.svg');
		expect(fileName({ ...png, mime_type: 'weird' })).toBe('aaaaaaaaaaaa');
	});
});
