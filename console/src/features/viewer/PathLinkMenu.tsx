// PathLinkMenuHost — the context menu for the file / folder links a Markdown body carries
// (`[text](rel/path)` in mdLinks.ts, inline-code paths in mdRefLinks.ts). The links are built
// imperatively, so the host hands them an opener through context, the way SessionLinkMenuHost
// does for session slugs.
//
// Opt-in by surface: MarkdownView wires a menu only under a host AND with an onOpenFile, so the
// shared-session view (a recipient has no such file) keeps the browser's native menu.
//
// Only non-destructive actions: the target is resolved from text an agent wrote, and rename /
// delete from inside a rendered document would be a surprise. The items shared with the Files
// rail live in project/PathMenuItems.tsx.
import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { createPortal } from "react-dom";
import { Icon } from "../../ui/Icon.tsx";
import { useT } from "../../lib/i18n/index.ts";
import { placeFixed } from "../../lib/placeFixed.ts";
import { useDismiss } from "../../lib/useDismiss.ts";
import { useMenuRoving } from "../../lib/useMenuRoving.ts";
import { useFilesStore } from "../files/store.ts";
import { PathMenuItems } from "../project/PathMenuItems.tsx";

/** A link target that has already been re-resolved against the file system. */
export interface PathLinkTarget {
  path: string;
  type: "file" | "dir";
  /** Opens a file in a pane; absent where the surface cannot open files. */
  openFile?: (openInNew: boolean) => void;
}

/** Opens the menu for `target` at viewport coordinates (x, y). */
export type OpenPathLinkMenu = (target: PathLinkTarget, x: number, y: number) => void;

const PathLinkMenuContext = createContext<OpenPathLinkMenu | null>(null);

/** The opener of the nearest host, or null where links keep the native menu. */
export function usePathLinkMenu(): OpenPathLinkMenu | null {
  return useContext(PathLinkMenuContext);
}

type MenuState = PathLinkTarget & { x: number; y: number };

export function PathLinkMenuHost({ children }: { children: ReactNode }) {
  const [menu, setMenu] = useState<MenuState | null>(null);
  const open = useCallback<OpenPathLinkMenu>((target, x, y) => setMenu({ ...target, x, y }), []);
  return (
    <PathLinkMenuContext.Provider value={open}>
      {children}
      {menu && <PathLinkMenu menu={menu} onClose={() => setMenu(null)} />}
    </PathLinkMenuContext.Provider>
  );
}

function PathLinkMenu({ menu, onClose }: { menu: MenuState; onClose: () => void }) {
  const tr = useT();
  const ref = useRef<HTMLUListElement>(null);
  useDismiss(ref, true, onClose);
  useMenuRoving(ref, true);
  useEffect(() => {
    window.addEventListener("blur", onClose);
    return () => window.removeEventListener("blur", onClose);
  }, [onClose]);
  // Re-placed on every render, before paint, like the rail menu.
  useLayoutEffect(() => {
    if (ref.current) placeFixed(ref.current, menu.x, menu.y);
  });
  // Opened by the keyboard, the menu took focus from the link; give it back on close, but only
  // when nothing else has claimed it (a click elsewhere keeps the user's choice).
  useLayoutEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    if (!opener || opener === document.body) return;
    return () => {
      const now = document.activeElement;
      if ((!now || now === document.body) && opener.isConnected) opener.focus();
    };
  }, []);

  const run = (fn: () => void) => {
    onClose();
    fn();
  };
  // The Files rail is rooted at home: a path outside it has nowhere to be revealed.
  const revealable = !menu.path.startsWith("/");
  const { path, type, openFile } = menu;

  return createPortal(
    <ul
      className="ui-menu files-ctxmenu"
      ref={ref}
      style={{ left: menu.x, top: menu.y }}
      role="menu"
      onMouseDown={(e) => e.stopPropagation()}
    >
      {type === "file" && openFile && (
        <>
          <li>
            <button type="button" className="ui-menu-item" onClick={() => run(() => openFile(false))}>
              <Icon name="file" /> {tr("view.path_menu.open")}
            </button>
          </li>
          <li>
            <button type="button" className="ui-menu-item" onClick={() => run(() => openFile(true))}>
              <Icon name="split-horizontal" /> {tr("view.path_menu.open_new")}
            </button>
          </li>
        </>
      )}
      {revealable && (
        <li>
          <button
            type="button"
            className="ui-menu-item"
            onClick={() => run(() => useFilesStore.getState().revealInFiles(path, { focus: true }))}
          >
            <Icon name="folder-opened" /> {tr("view.path_menu.reveal")}
          </button>
        </li>
      )}
      <PathMenuItems path={path} type={type} runMenu={run} closeMenu={onClose} />
    </ul>,
    document.body,
  );
}
