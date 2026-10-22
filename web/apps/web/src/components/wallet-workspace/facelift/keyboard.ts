export function isTypingTarget(target: EventTarget | null): boolean {
  return (
    target instanceof HTMLElement &&
    (target.tagName === "INPUT" ||
      target.tagName === "TEXTAREA" ||
      target.tagName === "SELECT" ||
      target.isContentEditable)
  );
}

export function isEscapeGuardedTarget(target: EventTarget | null): boolean {
  if (!isTypingTarget(target)) {
    return false;
  }
  return !(
    target instanceof HTMLInputElement &&
    (target.inputMode === "decimal" ||
      target.inputMode === "numeric" ||
      target.type === "number")
  );
}
