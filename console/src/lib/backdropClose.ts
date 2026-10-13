import { useRef } from "react";
import type { MouseEvent, PointerEvent } from "react";

// useBackdropClose — props for an overlay backdrop that dismisses on a click
// landing on the backdrop itself. A click is dispatched to the nearest common
// ancestor of the mousedown and mouseup targets, so a text selection dragged out
// of the panel and released over the backdrop arrives as a click whose target is
// the backdrop; the panel never sees it and its stopPropagation cannot help.
// The reverse drag (backdrop → panel) yields the same click, so both the press
// and the release have to be on the backdrop.
//
// Pointer events rather than mouse events: a pointerdown that some listener
// preventDefaults suppresses the compatibility mousedown/mouseup but not the
// click, which would leave a touch or pen tap unable to close. Only the primary
// button of the primary pointer counts, and every press starts from a clean
// state — a right-click fires no click to clear the flags, so recording it would
// let a later stray click close the dialog. Touch implicitly captures the
// pointer to where the press began, so a touch dragged out of the panel reports
// its pointerup on the panel and stays open.
export function useBackdropClose(onClose: (() => void) | undefined, active = true) {
  const down = useRef(false);
  const up = useRef(false);
  const primary = (e: PointerEvent) => e.isPrimary && e.button === 0 && e.target === e.currentTarget;
  return {
    onPointerDown: (e: PointerEvent) => {
      down.current = primary(e);
      up.current = false;
    },
    onPointerUp: (e: PointerEvent) => {
      up.current = down.current && primary(e);
    },
    onPointerCancel: () => {
      down.current = up.current = false;
    },
    onClick: (e: MouseEvent) => {
      const onBackdrop = down.current && up.current && e.target === e.currentTarget;
      down.current = up.current = false;
      if (active && onBackdrop) onClose?.();
    },
  };
}
