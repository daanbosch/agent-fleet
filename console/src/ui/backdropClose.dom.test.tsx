// A drag that starts inside a dialog and ends over its backdrop (a text selection) produces a
// click on the backdrop itself: the browser targets the common ancestor of mousedown and mouseup.
// jsdom does not synthesise that click, so each case dispatches pointerdown and pointerup where the
// press began and ended, and the click on their common ancestor as the browser would. No
// mousedown/mouseup is sent: a preventDefaulted pointerdown suppresses them, the click still comes.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { Modal } from "./Modal.tsx";
import { ConfirmDialog } from "./ConfirmDialog.tsx";
import { ConfirmProvider, useConfirm } from "./ConfirmProvider.tsx";

let host: HTMLDivElement;
let root: Root;

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
});

const backdrop = () => document.querySelector(".ui-modal-backdrop") as HTMLElement;

const pointer = (type: string, button = 0) =>
  new PointerEvent(type, { bubbles: true, isPrimary: true, pointerId: 1, button });

async function press(downOn: Element, upOn: Element) {
  let clickOn: Node = downOn;
  while (!clickOn.contains(upOn)) clickOn = clickOn.parentNode!;
  await act(async () => {
    downOn.dispatchEvent(pointer("pointerdown"));
    upOn.dispatchEvent(pointer("pointerup"));
    clickOn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  });
}

// Every overlay must behave the same; `inner` is an element inside its panel.
const cases: Array<{ name: string; mount: (onClose: () => void) => Promise<void>; inner: () => Element }> = [
  {
    name: "Modal",
    mount: async (onClose) => {
      await act(async () =>
        root.render(
          <Modal title="t" onClose={onClose}>
            <div className="ui-modal-body">
              <p className="body">selectable text</p>
            </div>
          </Modal>,
        ),
      );
    },
    inner: () => document.querySelector(".body")!,
  },
  {
    name: "ConfirmDialog",
    mount: async (onClose) => {
      await act(async () =>
        root.render(
          <ConfirmDialog title="t" onCancel={onClose}>
            <p className="body">selectable text</p>
          </ConfirmDialog>,
        ),
      );
    },
    inner: () => document.querySelector(".body")!,
  },
  {
    name: "ConfirmProvider",
    mount: async (onClose) => {
      let ask!: ReturnType<typeof useConfirm>;
      function Grab() {
        ask = useConfirm();
        return null;
      }
      await act(async () =>
        root.render(
          <ConfirmProvider>
            <Grab />
          </ConfirmProvider>,
        ),
      );
      await act(async () => {
        void ask({ title: "t", body: <p className="body">selectable text</p> }).then((ok) => {
          if (!ok) onClose();
        });
      });
    },
    inner: () => document.querySelector(".body")!,
  },
];

describe.each(cases)("$name backdrop", ({ mount, inner }) => {
  it("closes on a click pressed and released on the backdrop", async () => {
    const onClose = vi.fn();
    await mount(onClose);
    await press(backdrop(), backdrop());
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("stays open when a press inside the panel is released over the backdrop", async () => {
    const onClose = vi.fn();
    await mount(onClose);
    await press(inner(), backdrop());
    expect(onClose).not.toHaveBeenCalled();
  });

  it("stays open when a press on the backdrop is released inside the panel", async () => {
    const onClose = vi.fn();
    await mount(onClose);
    await press(backdrop(), inner());
    expect(onClose).not.toHaveBeenCalled();
  });

  it("does not let a right-click on the backdrop arm a later bare click", async () => {
    const onClose = vi.fn();
    await mount(onClose);
    // A right-click fires pointerdown/pointerup but no click, so nothing clears the state.
    await act(async () => {
      backdrop().dispatchEvent(pointer("pointerdown", 2));
      backdrop().dispatchEvent(pointer("pointerup", 2));
    });
    await act(async () => backdrop().click());
    expect(onClose).not.toHaveBeenCalled();
  });
});

describe("Modal backdrop with lockClose", () => {
  it("does not close while locked", async () => {
    const onClose = vi.fn();
    await act(async () => root.render(<Modal title="t" onClose={onClose} lockClose />));
    await press(backdrop(), backdrop());
    expect(onClose).not.toHaveBeenCalled();
  });
});
