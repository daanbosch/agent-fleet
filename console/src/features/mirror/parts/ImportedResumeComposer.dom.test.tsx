// Render test for the composer of a host session the console cannot attach to. Only fetch is
// replaced: the request goes through the real API client, so the URL, the body and the error
// text the operator reads are what the page ships.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

const { ImportedResumeComposer } = await import("./ImportedResumeComposer.tsx");

let root: Root | null = null;
let host: HTMLDivElement;
let fetchMock: ReturnType<typeof vi.fn>;

const field = () => host.querySelector<HTMLTextAreaElement>("textarea")!;
const submit = () => host.querySelector<HTMLButtonElement>("button[type=submit]")!;
const text = () => host.textContent ?? "";
const tick = () => new Promise((r) => setTimeout(r, 0));
const respond = (status: number, body: string) => Promise.resolve(new Response(body, { status }));

const render = async () => {
  await act(async () => {
    root!.render(<ImportedResumeComposer name="h-pi-0003" />);
  });
};

const typeText = async (value: string) => {
  await act(async () => {
    const set = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
    set.call(field(), value);
    field().dispatchEvent(new Event("input", { bubbles: true }));
  });
};

const press = async () => {
  await act(async () => {
    submit().click();
    await tick();
  });
};

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});

afterEach(() => {
  act(() => root?.unmount());
  host.remove();
  root = null;
  vi.unstubAllGlobals();
});

describe("ImportedResumeComposer", () => {
  it("keeps send disabled until the operator has typed a prompt", async () => {
    await render();
    expect(submit().disabled).toBe(true);
    await typeText("   ");
    expect(submit().disabled).toBe(true);
    await typeText("go on");
    expect(submit().disabled).toBe(false);
  });

  it("posts the trimmed prompt to the continue route and shows the id it got back", async () => {
    fetchMock.mockReturnValueOnce(respond(200, JSON.stringify({ name: "h-pi-0003", route: "session resume" })));
    await render();
    await typeText("  go on  ");
    await press();
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("api/sessions/h-pi-0003/continue");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body)).toEqual({ prompt: "go on", agent: "pi", mode: "normal" });
    expect(text()).toContain("h-pi-0003");
    expect(field().value).toBe("");
  });

  it("shows the host's refusal as the server wrote it and keeps the prompt to retry", async () => {
    const refusal = "continue is not available on this host: upgrade the hyprmux daemon";
    fetchMock.mockReturnValueOnce(respond(501, refusal));
    await render();
    await typeText("go on");
    await press();
    expect(text()).toContain(refusal);
    expect(field().value).toBe("go on");
  });

  it("holds the field and send while the request is in flight", async () => {
    let answer!: (r: Response) => void;
    fetchMock.mockReturnValueOnce(new Promise<Response>((r) => (answer = r)));
    await render();
    await typeText("go on");
    await press();
    expect(submit().disabled).toBe(true);
    expect(field().disabled).toBe(true);
    await act(async () => {
      answer(new Response(JSON.stringify({ name: "h-pi-0003", route: "session resume" }), { status: 200 }));
      await tick();
    });
    expect(field().disabled).toBe(false);
  });
});
