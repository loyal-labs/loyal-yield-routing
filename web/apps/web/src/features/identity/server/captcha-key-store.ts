import "server-only";

import { and, eq, gt, lt } from "drizzle-orm";
import { appCaptchaKeys } from "@loyal-labs/db-core/schema";

import { getDatabase } from "@/lib/core/database";

export async function putCaptchaKey(
  key: string,
  expiresAt: Date
): Promise<boolean> {
  const db = getDatabase();
  const [, inserted] = await db.batch([
    db.delete(appCaptchaKeys).where(lt(appCaptchaKeys.expiresAt, new Date())),
    db
      .insert(appCaptchaKeys)
      .values({ key, expiresAt })
      .onConflictDoNothing()
      .returning({ key: appCaptchaKeys.key }),
  ]);
  return inserted.length > 0;
}

export async function consumeCaptchaKey(key: string): Promise<boolean> {
  const db = getDatabase();
  const deleted = await db
    .delete(appCaptchaKeys)
    .where(
      and(eq(appCaptchaKeys.key, key), gt(appCaptchaKeys.expiresAt, new Date()))
    )
    .returning({ key: appCaptchaKeys.key });
  return deleted.length > 0;
}
