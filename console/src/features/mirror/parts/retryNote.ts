/** A provider error the agent is retrying, or gave up on (`failed`). Wire: /transcript's `retry`. */
export interface RetryNote {
  attempt: number;
  message: string;
  failed: boolean;
}

export function parseRetry(v: unknown): RetryNote | null {
  if (!v || typeof v !== "object") return null;
  const r = v as Record<string, unknown>;
  if (typeof r.message !== "string" || !r.message) return null;
  return {
    attempt: typeof r.attempt === "number" ? r.attempt : 0,
    message: r.message,
    failed: r.failed === true,
  };
}
