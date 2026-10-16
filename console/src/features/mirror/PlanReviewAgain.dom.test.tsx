// The "Review again in <reviewer>" button: beside "Review in another session", only when the
// hook names a live reviewer, on both the live and the carried card.
import { describe, it, expect, afterEach, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { PlanBlock } from "./transcript/blocks.tsx";
import { CarriedBlock } from "./CarriedBlock.tsx";
import { t } from "../../lib/i18n/index.ts";

let root: Root | null = null;
let host: HTMLElement;
afterEach(async () => {
  await act(async () => root?.unmount());
  host?.remove();
  root = null;
});
const mount = async (el: React.ReactElement) => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => root!.render(el));
};
const again = () => host.querySelector<HTMLButtonElement>(".mt-plan-review-again");

describe("live plan card", () => {
  const base = { plan: "# P\n\nbody", session: "planner", pending: true, onApprove: () => {}, onReject: () => {}, onReview: () => {} };
  it("shows no button without a reviewer", async () => {
    await mount(<PlanBlock {...base} onReviewAgain={() => {}} />);
    expect(again()).toBeNull();
  });
  it("shows it next to the launch button, names the reviewer, and calls back", async () => {
    const onReviewAgain = vi.fn();
    await mount(<PlanBlock {...base} onReviewAgain={onReviewAgain} reviewAgainName="Review: X" />);
    expect(again()!.textContent).toContain(t("plan.review_again", { name: "Review: X" }));
    expect(again()!.title).toBe(t("plan.review_again_title", { name: "Review: X" }));
    expect(host.querySelector(".mt-plan-review")).not.toBeNull();
    await act(async () => again()!.click());
    expect(onReviewAgain).toHaveBeenCalledTimes(1);
  });
  it("is blocked like the launch button while the session cannot take a send", async () => {
    await mount(<PlanBlock {...base} onReviewAgain={() => {}} reviewAgainName="R" sendDisabled="stopped" />);
    expect(again()!.disabled).toBe(true);
  });
});

describe("carried plan card", () => {
  const carried = { kind: "plan" as const, plan: "# P\n\nbody" };
  const props = { carried, session: "planner", agentName: "claude", onError: () => {}, onDone: () => {}, translate: (() => {}) as never };
  it("offers it only with a reviewer name, and passes the plan", async () => {
    await mount(<CarriedBlock {...props} onReviewPlan={async () => {}} onReviewPlanAgain={async () => {}} />);
    expect(again()).toBeNull();
    const fn = vi.fn(async () => {});
    await act(async () => root!.unmount());
    host.remove();
    await mount(<CarriedBlock {...props} onReviewPlan={async () => {}} onReviewPlanAgain={fn} reviewAgainName="Review: X" />);
    await act(async () => again()!.click());
    expect(fn).toHaveBeenCalledWith("# P\n\nbody");
  });
});
