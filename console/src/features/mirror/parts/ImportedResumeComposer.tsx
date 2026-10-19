import { useState } from "react";
import { t as tr } from "../../../lib/i18n/index.ts";
import { resumeImportedSession } from "./importedResume.ts";

type Outcome = { ok: true; name: string } | { ok: false; message: string };

// The composer for a host session the console cannot attach to. A send is a continue on the
// host; there is no optimistic transcript, so the reply appears on a later transcript poll.
export function ImportedResumeComposer({ name }: { name: string }) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const prompt = text.trim();

  const send = async () => {
    setBusy(true);
    setOutcome(null);
    try {
      const res = await resumeImportedSession(name, { prompt, agent: "pi", mode: "normal" });
      if ("error" in res) {
        setOutcome({ ok: false, message: res.error.message });
      } else {
        setOutcome({ ok: true, name: res.name });
        setText("");
      }
    } catch (e) {
      setOutcome({ ok: false, message: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <form
      className="mirror-compose mirror-compose-imported"
      onSubmit={(e) => {
        e.preventDefault();
        if (prompt && !busy) void send();
      }}
    >
      <textarea
        aria-label={tr("mirror.imported_placeholder")}
        placeholder={tr("mirror.imported_placeholder")}
        value={text}
        rows={2}
        disabled={busy}
        onChange={(e) => setText(e.target.value)}
      />
      <button type="submit" className="btn primary" disabled={busy || prompt === ""}>
        {busy ? tr("mirror.imported_sending") : tr("mirror.imported_send")}
      </button>
      {outcome?.ok === true && (
        <p role="status">
          {tr("mirror.imported_sent")} <code>{outcome.name}</code>
        </p>
      )}
      {outcome?.ok === false && <p role="alert">{`${tr("mirror.imported_failed")} ${outcome.message}`}</p>}
    </form>
  );
}
