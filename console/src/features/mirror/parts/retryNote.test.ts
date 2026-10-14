import { describe, expect, it } from "vitest";
import { parseRetry } from "./retryNote.ts";

describe("parseRetry", () => {
  it("reads a live retry", () => {
    expect(parseRetry({ attempt: 5, message: "Upstream request failed", nextMs: 1 })).toEqual({
      attempt: 5,
      message: "Upstream request failed",
      failed: false,
    });
  });
  it("reads the retry that ended the turn", () => {
    expect(parseRetry({ attempt: 2, message: "quota", failed: true })?.failed).toBe(true);
  });
  it("ignores anything else", () => {
    expect(parseRetry(undefined)).toBeNull();
    expect(parseRetry({ attempt: 1 })).toBeNull();
    expect(parseRetry("x")).toBeNull();
  });
});
