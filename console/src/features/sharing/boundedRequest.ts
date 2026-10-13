// boundedRequest gives a sharing request a deadline and a cancel handle.
//
// A share change waits on the owner's Workspace (the Control Plane reconciles the session
// catalog first, and /repos runs git per working copy), so for a member with many sessions
// it can take seconds. A dialog that locks itself on such a request with no deadline leaves
// the user with a spinner only a reload clears; every caller here either times out or lets
// the user walk away.

/** How long a share create / unshare / permission change / recipient lookup may take. */
export const SHARE_REQUEST_TIMEOUT_MS = 30_000;

export interface BoundedRequest {
  signal: AbortSignal;
  /** Abort because the user left (closed the dialog). */
  cancel: () => void;
  /** Stop the deadline once the request settled. */
  settle: () => void;
  /** True when the deadline, not the user, aborted it. */
  timedOut: () => boolean;
}

export function boundedRequest(ms = SHARE_REQUEST_TIMEOUT_MS): BoundedRequest {
  const ctl = new AbortController();
  let expired = false;
  const timer = setTimeout(() => {
    expired = true;
    ctl.abort();
  }, ms);
  return {
    signal: ctl.signal,
    cancel: () => {
      clearTimeout(timer);
      ctl.abort();
    },
    settle: () => clearTimeout(timer),
    timedOut: () => expired,
  };
}
