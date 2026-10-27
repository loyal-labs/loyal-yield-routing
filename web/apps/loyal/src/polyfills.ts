import { Buffer } from "buffer";

// Match the Solana Buffer support injected by the production build in Vite dev.
globalThis.Buffer ??= Buffer;
