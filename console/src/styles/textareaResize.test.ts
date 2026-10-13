// A textarea left at the browser default (`resize: both`) can be dragged wider than its modal
// (#2056). base.css makes vertical the app-wide default; jsdom does not apply CSS, so the rule
// text is checked instead.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const SRC = path.join(path.dirname(fileURLToPath(import.meta.url)), "..");
const stripComments = (css: string) => css.replace(/\/\*[\s\S]*?\*\//g, "");

const cssFiles = (dir: string): string[] =>
  fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) return cssFiles(p);
    return e.name.endsWith(".css") ? [p] : [];
  });

describe("textarea resizing", () => {
  it("defaults every textarea to vertical resizing", () => {
    const css = stripComments(fs.readFileSync(path.join(SRC, "styles/base.css"), "utf8"));
    expect(css).toMatch(/(^|\})\s*textarea\s*\{[^}]*resize:\s*vertical;/);
  });

  it("never lets a rule bring horizontal resizing back", () => {
    const files = cssFiles(SRC);
    expect(files.length).toBeGreaterThan(10);
    const offenders = files.filter((f) => /resize:\s*(both|horizontal|inline)\b/.test(stripComments(fs.readFileSync(f, "utf8"))));
    expect(offenders.map((f) => path.relative(SRC, f))).toEqual([]);
  });
});
