import { fileURLToPath } from "node:url";

import { nextSpa } from "../loyal/vite.preset";

const here = (path: string) => fileURLToPath(new URL(path, import.meta.url));

export default nextSpa({
  nextApp: here("./"),
  router: false,
  devPort: 5175,
  devProxy: ["/api", "/readyz"],
});
