// The view-nav shared by the two memory entries (Agent memory, Memory snapshots), and the hook
// that decides which view is showing.
//
// A view is picked, in this order, by a deep link ("memory:changes" through openSettings), by the
// view last left open in this entry (device-local, like the rail section), then by the entry's
// own default. The views themselves are standalone components, so this nav is the only thing
// that ties them to the Settings modal.

import { useCallback, useEffect, useState } from "react";
import { useSettingsUI, lastSectionView, rememberSectionView } from "../store.ts";

export interface ViewDef {
  key: string;
  label: string;
}

/** Which view of `section` is showing. `dflt` is the entry's default when nothing was asked or
 *  remembered; it is NOT stored, so a default forced by the state of the workspace never
 *  overwrites what the member chose. */
export function useSectionView(
  section: string,
  views: readonly string[],
  dflt: string,
  useRemembered = true,
): [string, (view: string) => void] {
  const requested = useSettingsUI((s) => s.settingsView);
  const clearRequested = useSettingsUI((s) => s.clearSettingsView);
  const [view, setView] = useState(() => {
    const ask = useSettingsUI.getState().settingsView;
    if (ask && views.includes(ask)) return ask;
    const last = useRemembered ? lastSectionView(section) : "";
    return views.includes(last) ? last : dflt;
  });

  const pick = useCallback(
    (v: string) => {
      setView(v);
      rememberSectionView(section, v);
    },
    [section],
  );

  // A request made while this entry is already on screen. A name that is no view of this entry is
  // dropped, so it cannot linger and fire on a later, unrelated mount.
  useEffect(() => {
    if (!requested) return;
    if (views.includes(requested)) pick(requested);
    clearRequested();
  }, [requested, views, pick, clearRequested]);

  return [view, pick];
}

/** Tabs at desktop widths, a select at phone width (the CSS shows one of the two). */
export function ViewNav({
  label,
  views,
  value,
  onChange,
  prefix,
}: {
  label: string;
  views: readonly ViewDef[];
  value: string;
  onChange: (view: string) => void;
  /** Id prefix tying each tab to the panel it controls (`${prefix}-panel`). */
  prefix: string;
}) {
  return (
    <div className="mem-viewnav">
      <div className="mem-viewnav-tabs" role="tablist" aria-label={label}>
        {views.map((v) => (
          <button
            key={v.key}
            type="button"
            role="tab"
            id={`${prefix}-tab-${v.key}`}
            aria-selected={v.key === value}
            aria-controls={`${prefix}-panel`}
            className={"mem-viewnav-tab" + (v.key === value ? " active" : "")}
            onClick={() => onChange(v.key)}
          >
            {v.label}
          </button>
        ))}
      </div>
      <select
        className="cinput mem-viewnav-select"
        aria-label={label}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      >
        {views.map((v) => (
          <option key={v.key} value={v.key}>
            {v.label}
          </option>
        ))}
      </select>
    </div>
  );
}
