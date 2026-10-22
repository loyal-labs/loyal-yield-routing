"use client";

import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";

import { readCssDurationMs } from "@/components/wallet-workspace/facelift/css-duration";

export function SheetReveal({
  children,
  isOpen,
  onClose,
  scrimClassName,
  sheetClassName,
}: {
  children: ReactNode;
  isOpen: boolean;
  onClose: () => void;
  scrimClassName: string;
  sheetClassName: string;
}) {
  const scrimRef = useRef<HTMLDivElement>(null);
  const sheetRef = useRef<HTMLDivElement>(null);
  const timerRef = useRef<number | null>(null);
  const [isMounted, setIsMounted] = useState(false);
  if (isOpen && !isMounted) {
    setIsMounted(true);
  }

  useLayoutEffect(() => {
    const scrim = scrimRef.current;
    const sheet = sheetRef.current;
    if (!(scrim && sheet)) {
      return;
    }
    if (timerRef.current !== null) {
      window.clearTimeout(timerRef.current);
      timerRef.current = null;
    }
    if (isOpen) {
      scrim.dataset.state = "open";
      sheet.dataset.state = "open";
      return;
    }
    scrim.dataset.state = "closed";
    sheet.dataset.state = "closed";
    const closeDur = readCssDurationMs("--panel-close-dur", 350);
    timerRef.current = window.setTimeout(() => {
      timerRef.current = null;
      setIsMounted(false);
    }, closeDur);
  }, [isOpen, isMounted]);

  useEffect(
    () => () => {
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current);
      }
    },
    []
  );

  if (!isMounted) {
    return null;
  }
  // Portaled to <body>: the call sites live inside the page-slide wrappers,
  // whose child-motion rules and mid-slide transforms would interfere with a
  // fixed overlay — the chart overlay lives outside that subtree, and this
  // puts the sheets in the same position. isMounted is only ever set on the
  // client, so document is always defined here.
  return createPortal(
    <div
      className={`t-sheet-scrim ${scrimClassName}`}
      data-state="open"
      onClick={onClose}
      ref={scrimRef}
    >
      <div
        aria-modal="true"
        className={`t-sheet-panel ${sheetClassName}`}
        data-state="open"
        onClick={(event) => event.stopPropagation()}
        ref={sheetRef}
        role="dialog"
      >
        {children}
      </div>
    </div>,
    document.body
  );
}

export function InlineSheetReveal({
  children,
  className,
  isOpen,
}: {
  children: ReactNode;
  className: string;
  isOpen: boolean;
}) {
  const panelRef = useRef<HTMLDivElement>(null);
  const timerRef = useRef<number | null>(null);
  const [isMounted, setIsMounted] = useState(false);
  if (isOpen && !isMounted) {
    setIsMounted(true);
  }

  useLayoutEffect(() => {
    const panel = panelRef.current;
    if (!panel) {
      return;
    }
    if (timerRef.current !== null) {
      window.clearTimeout(timerRef.current);
      timerRef.current = null;
    }
    if (isOpen) {
      panel.dataset.state = "open";
      return;
    }
    panel.dataset.state = "closed";
    const closeDur = readCssDurationMs("--panel-close-dur", 350);
    timerRef.current = window.setTimeout(() => {
      timerRef.current = null;
      setIsMounted(false);
    }, closeDur);
  }, [isOpen, isMounted]);

  useEffect(
    () => () => {
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current);
      }
    },
    []
  );

  if (!isMounted) {
    return null;
  }
  return (
    <div
      className={`t-sheet-panel ${className}`}
      data-state="open"
      ref={panelRef}
    >
      {children}
    </div>
  );
}
