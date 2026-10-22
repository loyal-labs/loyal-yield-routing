import { requireNativeModule } from "expo-modules-core";

export type NativeVaultAccount = {
  authToken: string | number;
  derivationPath: string;
  publicKey: string;
};

type NativeAuthToken = string | number;

type NativeModule = {
  /** True on binaries whose bridge passes auth tokens as decimal strings. */
  stringAuthTokens?: boolean;
  isAvailable(): Promise<boolean>;
  requestPermission(): Promise<boolean>;
  authorizeExistingSeed(derivationPath: string): Promise<NativeVaultAccount>;
  listAuthorizedSeeds(
    derivationPath: string,
  ): Promise<NativeVaultAccount[]>;
  createNewSeed(derivationPath: string): Promise<NativeVaultAccount>;
  importSeed(derivationPath: string): Promise<NativeVaultAccount>;
  deauthorize(authToken: NativeAuthToken): Promise<void>;
  signTransaction(
    authToken: NativeAuthToken,
    derivationPath: string,
    txBase64: string,
  ): Promise<string>;
  signTransactions(
    authToken: NativeAuthToken,
    derivationPath: string,
    txsBase64: string[],
  ): Promise<string[]>;
  signMessage(
    authToken: NativeAuthToken,
    derivationPath: string,
    messageBase64: string,
  ): Promise<string>;
  getPublicKey(
    authToken: NativeAuthToken,
    derivationPath: string,
  ): Promise<string>;
};

export default requireNativeModule<NativeModule>("ExpoSeedVault");
