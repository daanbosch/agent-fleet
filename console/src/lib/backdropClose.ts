import { useRef } from "react";
import type { MouseEvent } from "react";

// useBackdropClose — props for an overlay backdrop that dismisses on a click
// landing on the backdrop itself. A click is dispatched to the nearest common
// ancestor of the mousedown and mouseup targets, so a text selection dragged out
// of the panel and released over the backdrop arrives as a click whose target is
// the backdrop; the panel never sees it and its stopPropagation cannot help.
// The reverse drag (backdrop → panel) yields the same click, so both the press
// and the release have to be on the backdrop.
export function useBackdropClose(onClose: (() => void) | undefined, active = true) {
  const down = useRef(false);
  const up = useRef(false);
  return {
    onMouseDown: (e: MouseEvent) => {
      down.current = e.target === e.currentTarget;
    },
    onMouseUp: (e: MouseEvent) => {
      up.current = e.target === e.currentTarget;
    },
    onClick: (e: MouseEvent) => {
      const onBackdrop = down.current && up.current && e.target === e.currentTarget;
      down.current = up.current = false;
      if (active && onBackdrop) onClose?.();
    },
  };
}
