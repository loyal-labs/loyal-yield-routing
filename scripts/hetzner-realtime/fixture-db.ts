import { SQL } from "bun";
import { fixtureDb } from "./fixture-guard";

// Explicit TCP parameters: no Neon HTTP adapter, URL overrides, DNS lookup of
// localhost, default database, or inherited PGHOST transport.
const pool = new SQL({
  hostname: fixtureDb.hostname === "[::1]" ? "::1" : "127.0.0.1",
  port: Number(fixtureDb.port),
  database: fixtureDb.pathname.slice(1),
  username: decodeURIComponent(fixtureDb.username),
  password: decodeURIComponent(fixtureDb.password),
  tls: false,
  max: 1,
  maxLifetime: 0,
  connectionTimeout: 5,
});
const sql = await pool.reserve();

// Verify on the same reserved TCP session used for writes. A marker prevents
// treating an arbitrary existing loopback database as disposable.
export async function fixtureSql() {
  const [identity] = await sql`
    SELECT current_database() AS database,
           host(inet_server_addr()) AS address,
           inet_server_port() AS port,
           shobj_description(oid, 'pg_database') AS marker
    FROM pg_database WHERE datname = current_database()
  `;
  if (identity?.database !== fixtureDb.pathname.slice(1) ||
      !["127.0.0.1", "::1"].includes(identity?.address) ||
      identity?.port !== Number(fixtureDb.port) ||
      identity?.marker !== "loyal-realtime-disposable-fixture") {
    throw new Error("Connected SQL endpoint is not the marked disposable loopback fixture");
  }
  return sql;
}

export async function closeFixtureDb() {
  sql.release();
  await pool.close({ timeout: 0 });
}
