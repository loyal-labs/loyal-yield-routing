"use client";

import { useEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";

import { readCssDurationMs } from "@/components/wallet-workspace/facelift/css-duration";

export function TextSwap({
  className,
  text,
}: {
  className?: string;
  text: string;
}) {
  const [displayText, setDisplayText] = useState(text);
  const spanRef = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    const el = spanRef.current;
    if (!el || el.textContent === text) {
      return;
    }
    const dur = readCssDurationMs("--text-swap-dur", 150);
    el.classList.add("is-exit");
    const timer = window.setTimeout(() => {
      flushSync(() => setDisplayText(text));
      el.classList.remove("is-exit");
      el.classList.add("is-enter-start");
      void el.offsetHeight;
      el.classList.remove("is-enter-start");
    }, dur);
    return () => window.clearTimeout(timer);
  }, [text]);

  return (
    <span className={`t-text-swap ${className ?? ""}`} ref={spanRef}>
      {displayText}
    </span>
  );
}
