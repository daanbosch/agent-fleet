// SessionLinks — a session row's second line (#1062): the pull request of the branch the
// session works on, and the ports its processes listen on. A port opens in the workspace's
// default target (subdomain, else browser pane, else lightweight preview — see features/preview).
//
// A line of its own rather than chips beside the title: the rail is narrow and the title is
// already the part that gets cut. A row with neither draws nothing and stays one line, so the
// height is only paid where there is something row-specific to say.
import { useState } from "react";
import { Icon } from "../../ui/Icon.tsx";
import { useT } from "../../lib/i18n/index.ts";
import { PortOpen } from "../preview/PortOpen.tsx";
import { CHECK_ICON, PR_ICON, prChecks, prStateKey, rowPorts, safePRURL } from "./sessionLinks.ts";
import type { Session } from "../../types/session.ts";

/** Ports drawn before the rest fold behind "+N": one line at the rail's narrowest. */
const PORTS_SHOWN = 3;

/** Whether a row has a link line to draw. */
export function hasSessionLinks(s: Session, running: boolean): boolean {
  return Boolean(s.pr && safePRURL(s.pr.url)) || (running && Boolean(s.alive) && rowPorts(s.ports).length > 0);
}

export function SessionLinks({ s, running }: { s: Session; running: boolean }) {
  const tr = useT();
  const [allPorts, setAllPorts] = useState(false);
  const pr = s.pr;
  const href = pr ? safePRURL(pr.url) : null;
  // A port is only worth offering while the Workspace runs: the pane cannot reach it otherwise.
  const ports = running && s.alive ? rowPorts(s.ports) : [];
  if (!(pr && href) && ports.length === 0) return null;

  return (
    <div className="sess-links" aria-label={tr("srow.links")}>
      {pr && href && (() => {
        const key = prStateKey(pr);
        const ci = prChecks(pr);
        return (
          <a
            className={"sess-pr pr-" + key}
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            title={tr("srow.pr_title", {
              n: pr.number,
              state: tr(`srow.pr_state.${key}`),
              checks: ci ? tr(`srow.pr_checks.${ci}`) : "",
            })}
          >
            <Icon name={PR_ICON[key]} />#{pr.number}
            {ci && <Icon name={CHECK_ICON[ci]} className={"sess-pr-ci ci-" + ci} />}
          </a>
        );
      })()}
      {ports.length > 0 && (
        <span className="sess-ports">
          <Icon name="globe" />
          {(allPorts ? ports : ports.slice(0, PORTS_SHOWN)).map((port) => (
            <PortOpen key={port} port={port} running={running} variant="chip" />
          ))}
          {!allPorts && ports.length > PORTS_SHOWN && (
            <button
              type="button"
              className="sess-port sess-port-more"
              title={tr("srow.ports_more", { n: ports.length - PORTS_SHOWN })}
              onClick={() => setAllPorts(true)}
            >
              +{ports.length - PORTS_SHOWN}
            </button>
          )}
        </span>
      )}
    </div>
  );
}
