import { type RefObject, useEffect, useRef } from "react";
import { isEscapeGuardedTarget } from "@/components/wallet-workspace/facelift/keyboard";

type EscapeHandler = (event: KeyboardEvent) => void;
type Entry = RefObject<EscapeHandler>;

const overlays: Entry[] = [];
const pages: Entry[] = [];

function useRegisteredEscape(
  list: Entry[],
  isActive: boolean,
  handler: EscapeHandler
) {
  const handlerRef = useRef(handler);
  handlerRef.current = handler;
  useEffect(() => {
    if (!isActive) {
      return;
    }
    list.push(handlerRef);
    return () => {
      list.splice(list.indexOf(handlerRef), 1);
    };
  }, [isActive, list]);
}

export function useEscapeOverlay(isOpen: boolean, onEscape: EscapeHandler) {
  useRegisteredEscape(overlays, isOpen, onEscape);
}

export function useEscapePage(isActive: boolean, onEscape: EscapeHandler) {
  useRegisteredEscape(pages, isActive, onEscape);
}

export function dispatchEscape(event: KeyboardEvent) {
  const overlay = overlays.at(-1);
  if (overlay) {
    event.preventDefault();
    overlay.current(event);
    return;
  }
  if (isEscapeGuardedTarget(event.target)) {
    return;
  }
  pages.at(-1)?.current(event);
}
