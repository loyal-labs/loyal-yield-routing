export function bestEffort<T>(run: () => T): T | undefined {
  try {
    return run();
  } catch {
    return undefined;
  }
}

export async function bestEffortAsync<T>(
  run: () => Promise<T>
): Promise<T | undefined> {
  try {
    return await run();
  } catch {
    return undefined;
  }
}
