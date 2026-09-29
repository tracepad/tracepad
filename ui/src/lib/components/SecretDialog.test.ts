import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import SecretDialog from './SecretDialog.svelte';

// The secret dialog shows the lines that fit the key's scopes (spec 045 #14):
// every key gets the Tracepad variables, which the packages export with too,
// and only a key that can ingest gets the exporter formats — a `read` key
// pasted into an exporter's headers is a mistake the dialog prevents by not
// suggesting it.

vi.mock('$app/state', () => ({ page: { url: new URL('http://tracepad.test/p/p1/settings') } }));

const PAIR = { public_key: 'tp-pk-new', secret_key: 'tp-sk-new' };

function show(scopes?: ('ingest' | 'read' | 'write')[]) {
	render(SecretDialog, { props: { pair: { ...PAIR, scopes }, onclose: () => {} } });
}

describe('the secret dialog', () => {
	it('gives an ingest key the Tracepad, OpenTelemetry and Langfuse lines', () => {
		show(['ingest']);

		const tracepad = screen.getByText(/TRACEPAD_API_KEY=tp-sk-new/).textContent;
		expect(tracepad).not.toContain('TRACEPAD_HOST');
		expect(tracepad).toContain('TRACEPAD_URL=http://tracepad.test');
		expect(screen.getByText(/OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http:\/\/tracepad.test\/v1\/traces/))
			.toBeTruthy();
		expect(screen.getByText(/LANGFUSE_SECRET_KEY=tp-sk-new/)).toBeTruthy();
	});

	it('gives a key without ingest only the Tracepad lines', () => {
		show(['read', 'write']);

		expect(screen.getByText(/TRACEPAD_API_KEY=tp-sk-new/)).toBeTruthy();
		expect(screen.queryByText(/OTEL_EXPORTER_OTLP/)).toBeNull();
		expect(screen.queryByText(/LANGFUSE_/)).toBeNull();
	});

	it('gives a read-only key one block to copy', () => {
		show(['read']);

		expect(screen.getAllByRole('button', { name: /^Copy the / })).toHaveLength(1);
		expect(screen.getByRole('button', { name: 'Copy the Tracepad packages, CLI and MCP settings' }))
			.toBeTruthy();
	});

	it("treats a pair without scopes as a new project's first key, which holds all three", () => {
		show(undefined);

		expect(screen.getByText(/TRACEPAD_API_KEY=tp-sk-new/)).toBeTruthy();
		expect(screen.getByText(/OTEL_EXPORTER_OTLP_HEADERS/)).toBeTruthy();
		expect(screen.getByText(/LANGFUSE_PUBLIC_KEY=tp-pk-new/)).toBeTruthy();
	});
});
