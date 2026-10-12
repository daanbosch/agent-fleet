// Opens Home in a pane (#2028), focusing the one already open: one Home per workspace.
import { useLayoutStore } from "../../layout/store.ts";

export function openHome(): void {
  useLayoutStore.getState().openTarget({ content: { kind: "home" } });
}
