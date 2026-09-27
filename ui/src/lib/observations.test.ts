import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
	OBSERVATION_TYPES,
	TYPE_ICONS,
	isObservationType,
	omittedNotice,
	typeIcon
} from './observations';

/**
 * The tree draws a kind per observation, and this is what keeps the drawing
 * and the vocabulary one: the document the server serves is read here, and a
 * type it can send that this build has no icon for fails. Same idea as the
 * filter parity test beside it, pointed at the type column.
 */
function documentedTypes(): string[] {
	const document = JSON.parse(
		readFileSync(resolve(process.cwd(), '../internal/server/openapi.json'), 'utf8')
	);
	return document.components.schemas.Observation.properties.type.enum as string[];
}

describe('the observation type vocabulary', () => {
	it('is the one the API documents', () => {
		expect([...OBSERVATION_TYPES].sort()).toEqual(documentedTypes().sort());
	});

	it('has an icon for every kind', () => {
		for (const type of OBSERVATION_TYPES) {
			expect(TYPE_ICONS[type], `no icon for ${type}`).toBeTruthy();
		}
		expect(Object.keys(TYPE_ICONS).sort()).toEqual([...OBSERVATION_TYPES].sort());
	});

	it('draws distinct kinds distinctly', () => {
		// Two kinds sharing an icon would make the column decorative rather
		// than informative.
		const icons = new Set(Object.values(TYPE_ICONS));
		expect(icons.size).toBe(OBSERVATION_TYPES.length);
	});

	it('falls back rather than leaving a hole', () => {
		// A server newer than this build can send a kind we have never heard
		// of; the row still has to draw.
		expect(isObservationType('workflow-step')).toBe(false);
		expect(typeIcon('workflow-step')).toBe(TYPE_ICONS.span);
		expect(typeIcon(undefined)).toBe(TYPE_ICONS.span);
	});
});

describe('the notice above a tree the server cut (spec 043 #18)', () => {
	it('says nothing about a whole tree', () => {
		expect(omittedNotice(null)).toBeNull();
		expect(omittedNotice({ observation_count: 3 })).toBeNull();
		expect(omittedNotice({ observation_count: 3, observations_omitted: 0 })).toBeNull();
	});

	it('names what is shown and what is not', () => {
		expect(omittedNotice({ observation_count: 10_001, observations_omitted: 1 })).toBe(
			'Showing the first 10,000 of 10,001 observations by start time; 1 is not shown.'
		);
		expect(omittedNotice({ observation_count: 50, observations_omitted: 19 })).toBe(
			'Showing the first 31 of 50 observations by start time; 19 are not shown.'
		);
	});
});
