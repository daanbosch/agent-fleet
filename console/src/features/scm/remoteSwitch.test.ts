import { describe, expect, it } from "vitest";
import { remoteSwitchTargets } from "./remoteSwitch.ts";

describe("remoteSwitchTargets", () => {
  it("splits on the first slash and keeps slashes in the branch", () => {
    expect(remoteSwitchTargets([{ name: "origin/feature/x", type: "remote" }])).toEqual([
      { ref: "origin/feature/x", remote: "origin", branch: "feature/x" },
    ]);
  });

  it("offers each remote's copy separately", () => {
    const got = remoteSwitchTargets([
      { name: "aaa/main", type: "remote" },
      { name: "origin/main", type: "remote" },
    ]);
    expect(got.map((t) => t.remote)).toEqual(["aaa", "origin"]);
  });

  it("skips symbolic HEAD, bare names, tags and branches already local here", () => {
    expect(
      remoteSwitchTargets([
        { name: "origin/HEAD", type: "remote" },
        { name: "origin", type: "remote" },
        { name: "v1", type: "tag" },
        { name: "main", type: "head" },
        { name: "origin/main", type: "remote" },
      ]),
    ).toEqual([]);
  });
});
