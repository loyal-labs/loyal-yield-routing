"use client";

import { useState } from "react";

export function TokenIconSwap({ src }: { src: string }) {
  const [slots, setSlots] = useState<{
    a: string;
    b: string;
    active: "a" | "b";
  }>({ a: src, b: src, active: "a" });
  const activeSrc = slots.active === "a" ? slots.a : slots.b;
  if (src !== activeSrc) {
    setSlots((current) =>
      current.active === "a"
        ? { ...current, b: src, active: "b" }
        : { ...current, a: src, active: "a" }
    );
  }
  return (
    <span className="t-icon-swap size-9 shrink-0" data-state={slots.active}>
      {/* eslint-disable-next-line @next/next/no-img-element */}
      <img
        alt=""
        aria-hidden="true"
        className="t-icon size-9 rounded-full object-cover"
        data-icon="a"
        src={slots.a}
      />
      {/* eslint-disable-next-line @next/next/no-img-element */}
      <img
        alt=""
        aria-hidden="true"
        className="t-icon size-9 rounded-full object-cover"
        data-icon="b"
        src={slots.b}
      />
    </span>
  );
}
