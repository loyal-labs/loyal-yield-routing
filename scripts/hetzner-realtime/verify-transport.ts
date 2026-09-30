import { fixtureSql, closeFixtureDb } from "./fixture-db";

const expectRejection = process.argv[2] === "reject-unmarked";
try {
  let sql;
  try {
    sql = await fixtureSql();
  } catch (error) {
    if (!expectRejection || !String(error).includes("not the marked disposable")) throw error;
    console.log(JSON.stringify({ unmarked_actual_database_rejected: true, writes_attempted: 0 }));
  }
  if (sql) {
    if (expectRejection) throw new Error("unmarked actual database was admitted");
    const [identity] = await sql`
      SELECT current_database() AS database, host(inet_server_addr()) AS address,
             inet_server_port() AS port
    `;
    console.log(JSON.stringify({ ...identity, writes_attempted: 0 }));
  }
} finally {
  await closeFixtureDb();
}
