import { useRef } from "react";

export function useLastValue<T>(value: T | null): T | null {
  const lastRef = useRef<T | null>(null);
  if (value !== null) {
    lastRef.current = value;
  }
  return value ?? lastRef.current;
}
