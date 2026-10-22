export type Base64 = string & { readonly __brand: "Base64" };

export function uint8ArrayToBase64(bytes: Uint8Array): Base64 {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary) as Base64;
}
