// Switch targets a commit's remote-tracking refs offer in the commit-graph menu.
import type { GraphRef } from "../../lib/gitgraph.ts";

export interface RemoteSwitchTarget {
  /** The ref as drawn, e.g. `origin/main`. */
  ref: string;
  remote: string;
  branch: string;
}

/** remoteSwitchTargets lists the `<remote>/<branch>` refs worth a "switch" entry:
 * the remote's symbolic HEAD is not a branch, and a branch that also has a local
 * head on the same commit is already offered as a plain switch. A remote name has
 * no slash but a branch name can, so only the first slash splits. */
export function remoteSwitchTargets(refs: GraphRef[]): RemoteSwitchTarget[] {
  const local = new Set(refs.filter((r) => r.type === "head").map((r) => r.name));
  const out: RemoteSwitchTarget[] = [];
  for (const r of refs) {
    if (r.type !== "remote") continue;
    const i = r.name.indexOf("/");
    if (i <= 0) continue;
    const branch = r.name.slice(i + 1);
    if (!branch || branch === "HEAD" || local.has(branch)) continue;
    out.push({ ref: r.name, remote: r.name.slice(0, i), branch });
  }
  return out;
}
