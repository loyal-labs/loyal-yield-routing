"use client";

import type { Get, ReferralFriend } from "@loyal-labs/api";
import { useCallback, useEffect, useRef, useState } from "react";

import { copyTextToClipboard } from "@/components/wallet-workspace/facelift/copy-text";
import { PaneReveal } from "@/components/wallet-workspace/facelift/pane-transitions";
import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";

const ASSET_BASE = "/wallet-workspace/facelift";

type Referrals = Get["/api/referrals"];

type Load =
  | { state: "loading" }
  | { state: "error" }
  | { state: "ready"; data: Referrals };

const STATUS_CHIPS: Record<
  ReferralFriend["status"],
  { background: string; dot: string; label: string }
> = {
  deposited: { background: "bg-positive/[0.14]", dot: "bg-positive", label: "Deposited" },
  joined: { background: "bg-accent-selected", dot: "bg-muted-foreground", label: "Joined" },
  withdrawn: { background: "bg-accent", dot: "bg-tertiary", label: "Withdrawn" },
};

const usd = new Intl.NumberFormat("en-US", {
  currency: "USD",
  style: "currency",
});

const joinDate = new Intl.DateTimeFormat("en-US", {
  day: "numeric",
  month: "short",
  timeZone: "America/Los_Angeles",
  year: "numeric",
});

function formatUsd(micro: number): string {
  return usd.format(micro / 1_000_000);
}

function formatJoinDate(iso: string): string {
  const parsed = new Date(iso);
  return Number.isNaN(parsed.getTime()) ? "" : joinDate.format(parsed);
}

function plural(count: number, one: string, many: string): string {
  return `${count} ${count === 1 ? one : many}`;
}

function summaryLine(data: Referrals): string {
  const joined = data.counts.joined + data.counts.deposited + data.counts.withdrawn;
  return [
    `${plural(joined, "friend", "friends")} joined`,
    `${data.counts.deposited} with a deposit`,
    `${formatUsd(data.deposited_micro)} deposited`,
  ].join(" · ");
}

function useReferrals(): { load: Load; retry: () => void } {
  const [load, setLoad] = useState<Load>({ state: "loading" });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoad({ state: "loading" });
    fetch("/api/referrals", { cache: "no-store", signal: controller.signal })
      .then(async (response) => {
        if (!response.ok) {
          throw new Error(`Referrals request failed: ${response.status}`);
        }
        return (await response.json()) as Referrals;
      })
      .then(
        (data) => setLoad({ data, state: "ready" }),
        () => {
          if (!controller.signal.aborted) {
            setLoad({ state: "error" });
          }
        }
      );
    return () => controller.abort();
  }, [attempt]);

  return { load, retry: () => setAttempt((count) => count + 1) };
}

function useCopied(): { copied: boolean; flash: () => void } {
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<number | null>(null);
  useEffect(
    () => () => {
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current);
      }
    },
    []
  );
  const flash = useCallback(() => {
    setCopied(true);
    if (timerRef.current !== null) {
      window.clearTimeout(timerRef.current);
    }
    timerRef.current = window.setTimeout(() => {
      timerRef.current = null;
      setCopied(false);
    }, 1500);
  }, []);
  return { copied, flash };
}

function InviteLink({ data }: { data: Referrals }) {
  const { copied, flash } = useCopied();

  const copy = async () => {
    if (await copyTextToClipboard(data.link)) {
      flash();
    }
  };

  const share = async () => {
    if (typeof navigator.share === "function") {
      try {
        await navigator.share({
          text: "Join me on Loyal",
          title: "Loyal",
          url: data.link,
        });
        return;
      } catch (error) {
        if (error instanceof DOMException && error.name === "AbortError") {
          return;
        }
      }
    }
    await copy();
  };

  return (
    <div className="flex w-full flex-col gap-3 rounded-2xl bg-accent p-4">
      <div className="flex flex-col gap-1">
        <span className="text-[13px] text-muted-foreground leading-4">
          Your invite code
        </span>
        <span
          className="font-semibold text-[28px] text-foreground leading-8 tracking-[0.04em]"
          data-invite-code
        >
          {data.code}
        </span>
      </div>
      <p
        className="min-w-0 truncate text-[14px] text-muted-foreground leading-5"
        data-invite-link
      >
        {data.link}
      </p>
      <div className="flex flex-wrap items-center gap-2">
        <button
          className="t-hover flex h-11 items-center gap-2 rounded-full bg-primary px-5 font-medium text-[14px] text-white leading-5 hover:bg-primary/90"
          onClick={copy}
          type="button"
        >
          <ThemedIcon
            className="size-5 text-white"
            src={`${ASSET_BASE}/${copied ? "icon-check.svg" : "icon-copy.svg"}`}
          />
          <span aria-live="polite">{copied ? "Copied" : "Copy link"}</span>
        </button>
        <button
          className="t-hover flex h-11 items-center rounded-full bg-accent-selected px-5 font-medium text-[14px] text-foreground leading-5 hover:bg-accent-selected/70"
          onClick={share}
          type="button"
        >
          Share
        </button>
      </div>
    </div>
  );
}

function FriendRow({ friend }: { friend: ReferralFriend }) {
  const chip = STATUS_CHIPS[friend.status];
  return (
    <li
      className="flex min-h-[60px] items-center gap-3 rounded-2xl px-4 py-2"
      data-friend-status={friend.status}
    >
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="truncate font-medium text-[16px] text-foreground leading-5">
          {friend.label}
        </span>
        <span className="text-[13px] text-muted-foreground leading-4">
          Joined {formatJoinDate(friend.bound_at)}
        </span>
      </div>
      {friend.status === "deposited" ? (
        <span className="shrink-0 font-medium text-[16px] text-foreground leading-5">
          {formatUsd(friend.deposited_micro)}
        </span>
      ) : null}
      <span
        className={`inline-flex w-[92px] shrink-0 items-center justify-center gap-1.5 rounded-md px-1.5 py-0.5 font-medium text-[12px] text-foreground leading-4 ${chip.background}`}
      >
        <span aria-hidden="true" className={`size-1.5 rounded-full ${chip.dot}`} />
        {chip.label}
      </span>
    </li>
  );
}

function Skeleton() {
  return (
    <div className="t-skel-rows flex flex-col gap-2 py-1">
      {[0, 1, 2].map((index) => (
        <div className="h-[60px] w-full rounded-2xl bg-accent" key={index} />
      ))}
    </div>
  );
}

function Body({ load, retry }: { load: Load; retry: () => void }) {
  if (load.state === "loading") {
    return <Skeleton />;
  }
  if (load.state === "error") {
    return (
      <div className="flex flex-col items-start gap-3 px-4 py-6" role="alert">
        <p className="text-[14px] text-muted-foreground leading-5">
          Couldn&apos;t load your invites.
        </p>
        <button
          className="t-hover flex h-11 items-center rounded-full bg-accent-selected px-5 font-medium text-[14px] text-foreground leading-5 hover:bg-accent-selected/70"
          onClick={retry}
          type="button"
        >
          Try again
        </button>
      </div>
    );
  }
  const { data } = load;
  return (
    <>
      <InviteLink data={data} />
      {data.friends.length === 0 ? (
        <p className="px-4 py-8 text-center text-[14px] text-muted-foreground leading-5">
          Anyone who joins Loyal with your link shows up here, with what they
          deposit.
        </p>
      ) : (
        <>
          <p
            className="px-4 text-[14px] text-muted-foreground leading-5"
            data-invite-summary
          >
            {summaryLine(data)}
          </p>
          <ul className="flex w-full flex-col">
            {data.friends.map((friend, index) => (
              <FriendRow friend={friend} key={`${friend.bound_at}-${index}`} />
            ))}
          </ul>
        </>
      )}
    </>
  );
}

export function InvitePage({ onBack }: { onBack: () => void }) {
  const { load, retry } = useReferrals();
  return (
    <div className="flex h-full min-h-0 min-w-0 flex-1 gap-2 p-2 max-[795px]:gap-0 max-[795px]:p-0">
      <PaneReveal>
        <section className="flex min-h-0 w-full flex-1 flex-col overflow-y-auto rounded-3xl bg-card max-[795px]:rounded-none">
          <header className="flex w-full shrink-0 items-center p-2">
            <button
              aria-label="Back"
              className="t-hover hidden size-11 shrink-0 items-center justify-center rounded-3xl hover:bg-accent max-[795px]:flex"
              onClick={onBack}
              type="button"
            >
              <ThemedIcon
                className="size-6 text-muted-foreground"
                src={`${ASSET_BASE}/icon-arrow-left.svg`}
              />
            </button>
            <div className="flex min-w-0 flex-1 items-center py-2.5 pl-4 max-[795px]:pl-1">
              <h1 className="truncate whitespace-nowrap font-semibold text-[20px] text-foreground leading-6 max-[795px]:text-[24px] max-[795px]:leading-7">
                Invite
              </h1>
            </div>
          </header>
          <div className="mx-auto flex w-full max-w-[640px] flex-col gap-4 px-4 pb-6">
            <Body load={load} retry={retry} />
          </div>
        </section>
      </PaneReveal>
    </div>
  );
}
