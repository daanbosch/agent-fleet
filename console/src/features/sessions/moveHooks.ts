// Reads the move dialog and the mirror's limit banner share: which kinds are connected, and how
// much quota the offered kinds have left. Both are display and ordering only; the Agent's move
// looks at neither.
import { useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { api } from "../../core/api/client.ts";
import { agentOf } from "../../agents/registry.ts";
import { getCachedConns, setCachedConns, subscribeConns } from "../repos/connsCache.ts";
import { MOVE_USAGE_ENDPOINTS, readMoveUsage, type MoveUsage } from "./move.ts";
import type { ConnectionsStatus, SessionKind } from "../../types/session.ts";

/**
 * The connection check the handoff dialog relies on: warm from the rail, fetched only cold.
 * Null until it has answered (or when it failed), which callers read as "do not filter".
 */
export function useConnectedKinds(): ((k: SessionKind) => boolean) | null {
  const conns = useSyncExternalStore(subscribeConns, getCachedConns, getCachedConns);
  useEffect(() => {
    if (conns) return;
    let alive = true;
    api("api/connections")
      .then((d) => alive && setCachedConns(d && !d.error ? (d as ConnectionsStatus) : null))
      .catch(() => {});
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- once on mount; `conns` is the warm-cache guard
  }, []);
  return useMemo(() => (conns ? (k: SessionKind) => agentOf(k).available({ conns }) : null), [conns]);
}

// useMoveUsage reads, once per mount, the usage of every offered kind the Console already has a
// usage source for. Display only: the Agent's move does not look at quotas.
export function useMoveUsage(kinds: SessionKind[], tick = 0): Partial<Record<SessionKind, MoveUsage>> {
  const [out, setOut] = useState<Partial<Record<SessionKind, MoveUsage>>>({});
  const key = kinds.join(",");
  useEffect(() => {
    let alive = true;
    for (const k of kinds) {
      const ep = MOVE_USAGE_ENDPOINTS[k];
      if (!ep) continue;
      // A failed or empty re-read (an error body or a rejected fetch alike) drops the kind back to
      // "unknown" rather than keep a number that may have rolled over.
      const settle = (u: MoveUsage | null) => {
        if (!alive) return;
        setOut((o) => {
          const { [k]: _old, ...rest } = o;
          return u ? { ...rest, [k]: u } : rest;
        });
      };
      api(ep)
        .then((d) => settle(readMoveUsage(d)))
        .catch(() => settle(null));
    }
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed by the kind list's contents and the caller's refresh tick
  }, [key, tick]);
  return out;
}

