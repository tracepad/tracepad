import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Erasure } from "./api/client.svelte";
import {
  ERASURE_POLL_MS,
  ErasureWatch,
  describe as say,
  erased,
  settle,
  stage,
} from "./erasure.svelte";

// A user-data erasure as the screens say it (spec 044), and as they follow it
// while it runs on the server (spec 047 #18).

const getErasure = vi.fn();

vi.mock("./api/client.svelte", () => ({
  ApiError: class ApiError extends Error {
    constructor(
      public status: number,
      message: string,
    ) {
      super(message);
    }
  },
  api: { erasure: (...args: unknown[]) => getErasure(...args) },
}));

function erasure(overrides: Partial<Erasure> = {}): Erasure {
  return {
    id: "4f0c9d3e8a1b2c3d4e5f60718293a4b5",
    state: "running",
    phase: "parsed",
    user_id: "user-4711",
    dry_run: false,
    created_at: "2026-10-02T09:00:00Z",
    started_at: "2026-10-02T09:00:00Z",
    finished_at: null,
    progress: { traces_at_start: 20000, traces_deleted: 5123 },
    deleted: { traces: 5123 },
    compaction: { requested_at: null, expected_by: null },
    error: null,
    ...overrides,
  } as Erasure;
}

beforeEach(() => getErasure.mockReset());
afterEach(() => vi.useRealTimers());

describe("the erasure sentence", () => {
  it("names the traces and what the raw archive lost", () => {
    expect(
      erased("user-4711", {
        traces: 12,
        raw_spans: 252,
        raw_batches_rewritten: 38,
        raw_batches_deleted: 3,
      }),
    ).toBe(
      "Erased 12 traces belonging to user-4711, and 252 spans from 41 raw batches.",
    );
  });

  it("says nothing about the archive when it lost nothing", () => {
    expect(erased("u", { traces: 1, raw_spans: 0 })).toBe(
      "Erased 1 trace belonging to u.",
    );
    expect(erased("u", {})).toBe("Erased 0 traces belonging to u.");
  });
});

describe("where an erasure is", () => {
  it("counts the parsed phase against the traces step 1 found", () => {
    expect(say(erasure(), "user-4711")).toBe(
      "Erasure in progress — parsed, 5,123 of 20,000 traces",
    );
  });

  it("names the phases it cannot count, and the state before one", () => {
    expect(
      stage(
        erasure({
          phase: "raw",
          progress: { traces_at_start: 20000, traces_deleted: 0 },
        }),
      ),
    ).toBe("raw");
    expect(stage(erasure({ phase: "tail" }))).toBe("tail");
    expect(stage(erasure({ state: "queued", phase: null }))).toBe("queued");
    expect(
      stage(
        erasure({ progress: { traces_at_start: null, traces_deleted: 0 } }),
      ),
    ).toBe("parsed");
  });

  it("ends in the sentence, or in why it failed and what it took first", () => {
    expect(
      say(erasure({ state: "done", phase: null, deleted: { traces: 3 } }), "u"),
    ).toBe("Erased 3 traces belonging to u.");
    expect(
      say(
        erasure({
          state: "failed",
          phase: null,
          error: "the disk is full",
          deleted: { traces: 2 },
        }),
        "u",
      ),
    ).toBe(
      "The erasure of u's data failed: the disk is full. Before it did, it erased 2 traces belonging to u.",
    );
  });
});

describe("a confirmed erasure", () => {
  it("that ended within the wait is its sentence", () => {
    const watch = new ErasureWatch();
    expect(
      settle(
        "p",
        "u",
        erasure({ state: "done", phase: null, deleted: { traces: 1 } }),
        watch,
      ),
    ).toBe("Erased 1 trace belonging to u.");
    expect(watch.running).toBe(false);
  });

  it("that failed within the wait is a failure", () => {
    expect(() =>
      settle(
        "p",
        "u",
        erasure({ state: "failed", phase: null, error: "boom" }),
        new ErasureWatch(),
      ),
    ).toThrow(/failed: boom/);
  });

  it("that runs on is followed every two seconds until it ends, and then left alone", async () => {
    vi.useFakeTimers();
    const watch = new ErasureWatch();
    expect(
      settle("p", "u", erasure({ state: "queued", phase: null }), watch),
    ).toMatch(/runs on the server/);
    expect(watch.running).toBe(true);

    getErasure
      .mockResolvedValueOnce(
        erasure({ progress: { traces_at_start: 20000, traces_deleted: 9000 } }),
      )
      .mockResolvedValueOnce(
        erasure({ state: "done", phase: null, deleted: { traces: 20000 } }),
      );
    await vi.advanceTimersByTimeAsync(ERASURE_POLL_MS);
    expect(say(watch.current!, "u")).toBe(
      "Erasure in progress — parsed, 9,000 of 20,000 traces",
    );
    await vi.advanceTimersByTimeAsync(ERASURE_POLL_MS);
    expect(watch.running).toBe(false);
    expect(say(watch.current!, "u")).toBe(
      "Erased 20000 traces belonging to u.",
    );

    await vi.advanceTimersByTimeAsync(10 * ERASURE_POLL_MS);
    expect(getErasure).toHaveBeenCalledTimes(2);
    expect(getErasure.mock.calls[0].slice(0, 2)).toEqual([
      "p",
      "4f0c9d3e8a1b2c3d4e5f60718293a4b5",
    ]);
  });

  it("stops being read when the screen stops following it, and keeps what it showed", async () => {
    vi.useFakeTimers();
    const watch = new ErasureWatch();
    watch.follow("p", erasure());
    watch.stop();
    await vi.advanceTimersByTimeAsync(5 * ERASURE_POLL_MS);
    expect(getErasure).not.toHaveBeenCalled();
    expect(watch.current?.state).toBe("running");
  });
});
