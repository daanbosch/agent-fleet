import { describe, it, expect } from "vitest";
import { FRAMEWORKS, fileText, renderFiles, visibleLines, type SnippetCtx } from "./devIdpSnippets.ts";

const base: SnippetCtx = {
  issuer: "https://af.example.invalid/dev-idp",
  discovery: "https://af.example.invalid/dev-idp/.well-known/openid-configuration",
  clientId: "afdev_abc",
  port: 8080,
  previewUrl: "https://slug-8080.example.invalid",
  values: "env",
  preview: true,
};
const combos = FRAMEWORKS.flatMap((fw) =>
  fw.variants.flatMap((v) =>
    (["env", "fill"] as const).flatMap((values) =>
      [true, false].map((preview) => ({ name: `${fw.id}/${v.id}/${values}/${preview ? "preview" : "plain"}`, v, c: { ...base, values, preview } })),
    ),
  ),
);
const all = (v: (typeof combos)[number]) =>
  renderFiles(v.v, v.c)
    .map((f) => fileText(f, v.c.preview))
    .join("\n");

describe("dev IdP snippets", () => {
  it.each(combos)("renders $name", (k) => {
    const files = renderFiles(k.v, k.c);
    expect(files.length).toBeGreaterThan(0);
    expect(files[0].lines.length).toBeGreaterThan(0);
  });

  it.each(combos)("never writes the client secret as a value: $name", (k) => {
    // The secret is not part of the context at all; a sentinel smuggled in must not surface.
    const leaky = { ...k, c: { ...k.c, clientSecret: "S3CRET-LEAK" } as SnippetCtx };
    expect(all(leaky)).not.toContain("S3CRET-LEAK");
    const text = all(k);
    expect(text).toContain("AF_DEV_IDP_CLIENT_SECRET");
    // On a secret line, once the variable names are removed no quoted literal is left.
    for (const l of text.split("\n").filter((x) => /secret/i.test(x))) {
      expect(l.replace(/"?\$?[A-Z][A-Z0-9_]*_SECRET"?/g, "").replace(/\$\{[^}]*\}/g, "")).not.toMatch(/["'][^"']+["']/);
    }
  });

  it("env mode holds nothing workspace-specific; fill mode holds this workspace's values", () => {
    for (const k of combos.filter((x) => x.c.values === "env")) {
      const t = all(k);
      expect(t).not.toContain("afdev_abc");
      expect(t).not.toContain("af.example.invalid");
    }
    for (const k of combos.filter((x) => x.c.values === "fill" && x.v.id !== "profile" && x.name.startsWith("spring/profile"))) {
      expect(all(k)).toContain("afdev_abc");
    }
    const fillSpring = combos.find((x) => x.name === "spring/profile/fill/plain")!;
    expect(all(fillSpring)).toContain("issuer-uri: https://af.example.invalid/dev-idp");
  });

  it("the port changes the preview URL in the snippet", () => {
    for (const k of combos.filter((x) => x.c.preview && x.c.values === "env")) {
      const a = all({ ...k, c: { ...k.c, port: 8080 } });
      const b = all({ ...k, c: { ...k.c, port: 3000 } });
      if (a.includes("AF_PREVIEW_URL_")) {
        expect(a).toContain("AF_PREVIEW_URL_8080");
        expect(b).toContain("AF_PREVIEW_URL_3000");
        expect(b).not.toContain("AF_PREVIEW_URL_8080");
      }
    }
    const goFill = combos.find((x) => x.name === "go/oidc/fill/preview")!;
    expect(all(goFill)).toContain("https://slug-8080.example.invalid");
  });

  it("include preview settings adds or removes the marked lines", () => {
    for (const fw of FRAMEWORKS) {
      for (const v of fw.variants) {
        const on = renderFiles(v, { ...base, preview: true });
        const off = renderFiles(v, { ...base, preview: false });
        const marked = on.flatMap((f) => f.lines.filter((l) => l.when === "preview"));
        expect(marked.length, `${fw.id}/${v.id}`).toBeGreaterThan(0);
        const offLines = new Set(off.flatMap((f) => visibleLines(f, false).map((l) => l.text)));
        const unmarked = new Set(on.flatMap((f) => f.lines.filter((l) => !l.when).map((l) => l.text)));
        for (const l of marked) if (!unmarked.has(l.text)) expect(offLines.has(l.text), l.text).toBe(false);
        // Turning it off drops lines (or swaps them), never leaves a preview-only file behind.
        for (const f of off) expect(visibleLines(f, false).length).toBeGreaterThan(0);
      }
    }
  });
});
