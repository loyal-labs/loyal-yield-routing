import { fileURLToPath } from "node:url";

import { nextSpa } from "../loyal/vite.preset";

const here = (path: string) => fileURLToPath(new URL(path, import.meta.url));

export default nextSpa({
  nextApp: here("./"),
  serverActions: { root: here("src/app/(admin)"), endpoint: "/api/admin/rpc", headers: { "X-Loyal-Admin": "1" } },
  serverOnly: { mode: "error", root: here("src") },
  devPort: 5174,
  devProxy: ["/api", "/auth", "/logout", "/readyz"],
});
