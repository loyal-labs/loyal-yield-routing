import { useEffect, useRef } from "react";

export function useAbandonActionsOnNavigate(
  navigationNonce: number,
  abandon: () => void
): void {
  const abandonRef = useRef(abandon);
  abandonRef.current = abandon;
  useEffect(() => {
    abandonRef.current();
  }, [navigationNonce]);
}
