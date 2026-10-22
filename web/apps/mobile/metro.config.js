const path = require("path");
const { getDefaultConfig } = require("expo/metro-config");
const { withNativewind } = require("nativewind/metro");

/** @type {import('expo/metro-config').MetroConfig} */
const config = getDefaultConfig(__dirname);

const sharedRoot = path.resolve(__dirname, "../../packages/shared");
const solanaRpcRoot = path.resolve(__dirname, "../../packages/solana-rpc/src");
const walletCoreRoot = path.resolve(__dirname, "../../packages/wallet-core/src");
const smartAccountVaultsRoot = path.resolve(
  __dirname,
  "../../packages/smart-account-vaults/src",
);
const loyalActionsRoot = path.resolve(__dirname, "../../packages/loyal-actions/src");
const loyalSmartAccountsRoot = path.resolve(
  __dirname,
  "../../packages/loyal-smart-accounts/src",
);
const loyalSmartAccountsCoreRoot = path.resolve(
  __dirname,
  "../../packages/loyal-smart-accounts-core/src",
);
const solanaWalletRoot = path.resolve(__dirname, "../../packages/solana-wallet/src");
const solanaInstructionDecoderRoot = path.resolve(
  __dirname,
  "../../packages/solana-instruction-decoder/src",
);
config.watchFolders = [
  sharedRoot,
  solanaRpcRoot,
  walletCoreRoot,
  smartAccountVaultsRoot,
  loyalActionsRoot,
  loyalSmartAccountsRoot,
  loyalSmartAccountsCoreRoot,
  solanaWalletRoot,
  solanaInstructionDecoderRoot,
];
config.resolver.nodeModulesPaths = [
  path.resolve(__dirname, "node_modules"),
  path.resolve(__dirname, "../../node_modules"),
];
config.resolver.extraNodeModules = {
  "@loyal-labs/solana-rpc": solanaRpcRoot,
  "@loyal-labs/wallet-core/hooks": path.resolve(walletCoreRoot, "hooks/index.ts"),
  "@loyal-labs/wallet-core/lib": path.resolve(walletCoreRoot, "lib/index.ts"),
  "@loyal-labs/smart-account-vaults": smartAccountVaultsRoot,
  "@loyal-labs/actions": loyalActionsRoot,
  "@loyal-labs/loyal-smart-accounts": loyalSmartAccountsRoot,
  "@loyal-labs/loyal-smart-accounts-core": loyalSmartAccountsCoreRoot,
  "@loyal-labs/loyal-smart-accounts-core/internal": path.resolve(
    loyalSmartAccountsCoreRoot,
    "internal/index.ts",
  ),
  "@loyal-labs/solana-wallet": solanaWalletRoot,
  "@loyal-labs/solana-instruction-decoder": solanaInstructionDecoderRoot,
};

const dedupedPackages = new Set([
  "@solana/web3.js",
]);

function packageNameOf(moduleName) {
  if (moduleName.startsWith(".") || path.isAbsolute(moduleName)) return null;
  if (!moduleName.startsWith("@")) return moduleName.split("/")[0];
  const parts = moduleName.split("/");
  return parts.length >= 2 ? `${parts[0]}/${parts[1]}` : null;
}

config.transformer.babelTransformerPath = require.resolve(
  "react-native-svg-transformer",
);
config.resolver.assetExts = config.resolver.assetExts.filter(
  (ext) => ext !== "svg",
);
config.resolver.sourceExts = [...config.resolver.sourceExts, "svg"];

const nativewindConfig = withNativewind(config, {
  inlineVariables: false,
  globalClassNamePolyfill: false,
  inlineRem: 16,
});

const nativewindResolveRequest = nativewindConfig.resolver.resolveRequest;
nativewindConfig.resolver.resolveRequest = (context, moduleName, platform) => {
  const dedupePackageName = packageNameOf(moduleName);
  if (dedupePackageName != null && dedupedPackages.has(dedupePackageName)) {
    return context.resolveRequest(
      { ...context, disableHierarchicalLookup: true },
      moduleName,
      platform,
    );
  }

  // The SDK's webcrypto.ts guards a `await import("node:crypto")` behind a
  // Node-only runtime check, but Metro still walks the import graph eagerly.
  // Shim it to an empty module so the bundler stops complaining; the guard
  // prevents the branch from ever executing on React Native.
  if (moduleName === "node:crypto") {
    return { type: "empty" };
  }

  // Privy pulls in `jose`, whose "import" export is a Node build that needs
  // "crypto"; its "browser" build uses WebCrypto and works on device. Do NOT
  // add "browser" to resolver.unstable_conditionNames for this: the Solana
  // MWA packages list "browser" before "react-native" in their exports, so
  // that setting silently swaps in their browser stubs and the Seeker wallet
  // button stops doing anything.
  if (moduleName === "jose") {
    return {
      type: "sourceFile",
      filePath: path.join(
        path.dirname(require.resolve("jose/package.json")),
        "dist/browser/index.js",
      ),
    };
  }

  const defaultResolve = (name) => {
    if (typeof nativewindResolveRequest === "function") {
      return nativewindResolveRequest(context, name, platform);
    }
    return context.resolveRequest(context, name, platform);
  };

  // The monorepo TS packages (loyal-smart-accounts-core etc.) use ESM
  // ".js"-suffixed relative imports that point at .ts sources (TypeScript
  // nodenext convention). Metro resolves specifiers literally, so when that
  // fails retry without the extension and let sourceExts find the .ts file.
  try {
    return defaultResolve(moduleName);
  } catch (error) {
    if (moduleName.startsWith(".") && moduleName.endsWith(".js")) {
      return defaultResolve(moduleName.slice(0, -3));
    }
    throw error;
  }
};

module.exports = nativewindConfig;
