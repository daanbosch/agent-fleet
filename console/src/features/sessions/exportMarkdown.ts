// "Export as Markdown" for a session's conversation (#2183). The Agent renders the whole
// transcript, not the mirror's window, and masks secret-looking strings.
//
// fetch -> Blob rather than a plain link: a stopped workspace or an unsupported kind answers
// with a JSON error, which a link navigation would save as the "Markdown" file.
import { errDetail, raw } from "../../core/api/client.ts";
import { t } from "../../lib/i18n/index.ts";

type Notify = (message: string, opts?: { kind?: "error" | "warn" | "info" | "success" }) => void;

export async function exportSessionMarkdown(name: string, tools: "names" | "full", notify: Notify): Promise<void> {
  let res: Response;
  try {
    // The browser's zone, so the file's times match the mirror's.
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone ?? "";
    res = await raw(
      "api/sessions/" + encodeURIComponent(name) + "/export?tools=" + tools + "&tz=" + encodeURIComponent(tz),
    );
  } catch {
    notify(t("srow.export_failed"));
    return;
  }
  if (!res.ok) {
    const body = await res.json().catch(() => null);
    notify(body?.error ? t("srow.export_failed") + ": " + errDetail(body.error) : t("srow.export_failed"));
    return;
  }
  const blob = await res.blob();
  const cd = res.headers.get("Content-Disposition") ?? "";
  const file = /filename="?([^";]+)"?/.exec(cd)?.[1] ?? name + ".md";
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = file;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
  notify(t("srow.export_done"), { kind: "success" });
}
