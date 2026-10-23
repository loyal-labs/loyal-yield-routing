export function sortByFundedNearestTrigger<T>(
  options: readonly T[],
  isFunded: (option: T) => boolean
): T[] {
  return [...options].sort(
    (a, b) => (isFunded(a) ? 1 : 0) - (isFunded(b) ? 1 : 0)
  );
}
