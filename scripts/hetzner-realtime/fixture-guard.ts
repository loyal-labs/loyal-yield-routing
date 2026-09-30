// Reject unsafe inputs before opening HTTP or SQL connections. Do not accept
// URL query options that could redirect the PostgreSQL transport.
export const fixtureDb = new URL(process.env.NEON_DATABASE_URL || "postgres://invalid");
export const fixtureEndpoint = new URL(process.env.REALTIME_URL || "http://invalid");
if (
  process.env.REALTIME_FIXTURE_ONLY !== "true" ||
  !["postgres:", "postgresql:"].includes(fixtureDb.protocol) ||
  !["localhost", "127.0.0.1", "[::1]"].includes(fixtureDb.hostname) ||
  !/^\/realtime_fixture(?:_[a-z0-9]+)?$/.test(fixtureDb.pathname) ||
  fixtureDb.search || fixtureDb.hash || !fixtureDb.username ||
  !fixtureDb.port || Number(fixtureDb.port) < 1 || Number(fixtureDb.port) > 65535 ||
  !["localhost", "127.0.0.1", "[::1]"].includes(fixtureEndpoint.hostname) ||
  fixtureEndpoint.protocol !== "http:" || fixtureEndpoint.username ||
  fixtureEndpoint.password || fixtureEndpoint.search || fixtureEndpoint.hash ||
  fixtureEndpoint.pathname !== "/" || !fixtureEndpoint.port ||
  Number(fixtureEndpoint.port) < 1 || Number(fixtureEndpoint.port) > 65535
) {
  throw new Error("Requires REALTIME_FIXTURE_ONLY=true, loopback realtime URL and disposable realtime_fixture DB without URL options");
}
