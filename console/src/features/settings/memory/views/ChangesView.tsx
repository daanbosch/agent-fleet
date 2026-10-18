// Agent memory > Changes: the change list with its diff and the way back.

import { AgentChangesPanel } from "../memoryChanges.tsx";

export function ChangesView({ reload, onChanged }: { reload: number; onChanged: () => void }) {
  return <AgentChangesPanel reload={reload} onChanged={onChanged} />;
}
