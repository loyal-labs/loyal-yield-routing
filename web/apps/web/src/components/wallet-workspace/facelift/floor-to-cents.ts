export function floorToCents(usd: number): string {
  return (Math.floor(usd * 100) / 100).toFixed(2);
}
