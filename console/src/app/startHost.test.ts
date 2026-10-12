// StartHost must be mounted in both of App's shells (docs/log/77 §77.18, run-sheet step H-17 on
// #1960): accepting a handover from a shared view in a popout tab opens the launch dialog only
// because the popout shell mounts it too, and without it the accept button does nothing.
// Checked on the source because rendering App needs most of the Console's backends.
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const src = readFileSync(new URL("./App.tsx", import.meta.url), "utf8");
const count = (s: string) => (s.match(/<StartHost \/>/g) || []).length;

describe("App mounts StartHost", () => {
  it("once in the pop-out shell and once in the full one", () => {
    const at = src.indexOf('if (popout === "popout") {');
    expect(at).toBeGreaterThan(0);
    const popoutEnd = src.indexOf("\n  }\n", at);
    expect(count(src.slice(at, popoutEnd))).toBe(1);
    expect(count(src.slice(popoutEnd))).toBe(1);
  });
});
