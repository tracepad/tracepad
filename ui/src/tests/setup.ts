import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/svelte';
import { afterEach } from 'vitest';

// Components mounted by one test must not still be in the document when the
// next one queries it.
afterEach(cleanup);
