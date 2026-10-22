export type WithdrawSubmitLock = {
  current: boolean;
};

export function acquireWithdrawSubmitLock(
  lock: WithdrawSubmitLock,
): (() => void) | null {
  if (lock.current) {
    return null;
  }

  lock.current = true;
  let released = false;
  return () => {
    if (released) {
      return;
    }
    released = true;
    lock.current = false;
  };
}
