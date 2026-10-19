import { apiJSON } from "../../../core/api/client.ts";

export interface ImportedResumeBody {
  prompt: string;
  agent: string;
  mode: string;
}

// A Pi host session resumes in place, so `name` is the source's own id. Any other source
// is continued into a new Pi session under a new id.
export type ImportedResumeResult =
  | { name: string; route: string }
  | { error: { code: string; message: string; status: number } };

export const resumeImportedSession = (name: string, body: ImportedResumeBody): Promise<ImportedResumeResult> =>
  apiJSON(`api/sessions/${encodeURIComponent(name)}/continue`, "POST", body);
