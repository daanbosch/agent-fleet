// Agent memory > Memories. The member's two lists over AF-owned memory, moved here unchanged;
// merging them into one master-detail is a later step.

import { MemoryPinsPanel } from "../memoryPins.tsx";
import { MemoryBrowserPanel } from "../memoryBrowser.tsx";

export function MemoriesView({ reload, onChanged }: { reload: number; onChanged: () => void }) {
  return (
    <>
      <MemoryPinsPanel reload={reload} onChanged={onChanged} />
      <MemoryBrowserPanel reload={reload} onChanged={onChanged} />
    </>
  );
}
