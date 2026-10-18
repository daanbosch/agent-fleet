// Agent memory > Import & export: every wizard that moves AF memory in or out of somewhere else,
// plus the project move, side by side as they were. They share no frame yet.

import { ClaudeImportPanel } from "../memoryClaudeImport.tsx";
import { MuseImportPanel } from "../memoryMuseImport.tsx";
import { ClaudeExportPanel } from "../memoryClaudeExport.tsx";
import { ProjectMovePanel } from "../memoryProjectMove.tsx";

export function ImportExportView({ reload, onChanged }: { reload: number; onChanged: () => void }) {
  return (
    <>
      <ClaudeImportPanel reload={reload} onChanged={onChanged} />
      <MuseImportPanel reload={reload} onChanged={onChanged} />
      <ClaudeExportPanel reload={reload} />
      <ProjectMovePanel reload={reload} onChanged={onChanged} />
    </>
  );
}
