// A textarea left at the browser default (`resize: both`) can be dragged wider than its modal
// (#2056). base.css makes vertical the app-wide default. The stylesheets are never loaded into
// the test DOM, so this checks the rule text: every `resize` declaration in console/src, CSS or
// a TSX inline style, has to be one that cannot widen the field. An allow-list rather than a
// deny-list, because `revert` / `unset` / `inherit` can bring the UA's `both` back as well.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const SRC = path.join(path.dirname(fileURLToPath(import.meta.url)), "..");
const stripComments = (src: string) => src.replace(/\/\*[\s\S]*?\*\//g, "");

const ALLOWED = new Set(["none", "vertical", "block"]);

/** Every `resize` value declared in `src`. In TS only a string literal counts (an inline style's
 *  `resize: "both"`): a bare `resize: x` there is a type or an unrelated config key. */
const resizeValues = (src: string, ts = false) =>
  [...stripComments(src).matchAll(ts ? /(?<![\w-])resize\s*:\s*["']([\w-]+)["']/gi : /(?<![\w-])resize\s*:\s*([\w-]+)/gi)].map(
    (m) => m[1].toLowerCase(),
  );

const sources = (dir: string): string[] =>
  fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) return sources(p);
    return /\.(css|tsx?)$/.test(e.name) && !/\.test\.tsx?$/.test(e.name) ? [p] : [];
  });

describe("textarea resizing", () => {
  it("reads resize declarations however they are spelled", () => {
    expect(resizeValues("textarea { resize : both; }")).toEqual(["both"]);
    expect(resizeValues("TEXTAREA { RESIZE: BOTH }")).toEqual(["both"]);
    expect(resizeValues("textarea{resize:revert}")).toEqual(["revert"]);
    expect(resizeValues(`<textarea style={{ resize: "horizontal" }} />`, true)).toEqual(["horizontal"]);
    expect(resizeValues("onResize: fit, max-resize: 2; /* resize: both */")).toEqual([]);
    expect(resizeValues("{ resize: HomeResize | null } // on every resize: whatever", true)).toEqual([]);
  });

  it("defaults every textarea to vertical resizing", () => {
    const css = stripComments(fs.readFileSync(path.join(SRC, "styles/base.css"), "utf8"));
    expect(css).toMatch(/(^|\})\s*textarea\s*\{[^}]*resize\s*:\s*vertical\s*;/i);
  });

  it("never lets a rule bring horizontal resizing back", () => {
    const files = sources(SRC);
    expect(files.filter((f) => f.endsWith(".css")).length).toBeGreaterThan(10);
    const offenders = files.flatMap((f) =>
      resizeValues(fs.readFileSync(f, "utf8"), !f.endsWith(".css"))
        .filter((v) => !ALLOWED.has(v))
        .map((v) => `${path.relative(SRC, f)}: resize ${v}`),
    );
    expect(offenders).toEqual([]);
  });
});
