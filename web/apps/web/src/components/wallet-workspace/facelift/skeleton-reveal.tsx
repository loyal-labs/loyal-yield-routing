"use client";

import type { ReactNode } from "react";

import {
  PopDigits,
  type PopDigitsSegment,
} from "@/components/wallet-workspace/facelift/pop-digits";

export function SkeletonReveal({
  children,
  isRevealed,
  skeletonClassName = "rounded-[6px] bg-accent-selected",
}: {
  children: ReactNode;
  isRevealed: boolean;
  skeletonClassName?: string;
}) {
  return (
    <span
      className={`t-skel t-skel-inline ${isRevealed ? "is-revealed" : ""}`}
      style={{ ["--pulse-count" as never]: "infinite" }}
    >
      <span aria-hidden="true" className="t-skel-skeleton is-pulsing">
        <span className={`block size-full ${skeletonClassName}`} />
      </span>
      <span className="t-skel-content">{children}</span>
    </span>
  );
}

function PlainSegments({ segments }: { segments: PopDigitsSegment[] }) {
  return (
    <>
      {segments.map((segment, index) =>
        segment.color ? (
          <span key={index} style={{ color: segment.color }}>
            {segment.text}
          </span>
        ) : (
          segment.text
        )
      )}
    </>
  );
}

export function ApyRevealText({
  isRevealed,
  segments,
  withPop = true,
}: {
  isRevealed: boolean;
  segments: PopDigitsSegment[];
  withPop?: boolean;
}) {
  return (
    <SkeletonReveal isRevealed={isRevealed}>
      {isRevealed && withPop ? (
        <PopDigits popOnChange={false} segments={segments} />
      ) : (
        <PlainSegments segments={segments} />
      )}
    </SkeletonReveal>
  );
}
