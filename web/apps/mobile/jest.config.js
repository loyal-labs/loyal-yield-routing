module.exports = {
  preset: "ts-jest",
  testEnvironment: "node",
  moduleNameMapper: {
    "^@/(.*)$": "<rootDir>/src/$1",
    "^@loyal-labs/shared$": "<rootDir>/../../packages/shared/src/index",
    "^@loyal-labs/wallet-core/lib$":
      "<rootDir>/../../packages/wallet-core/src/lib/index",
    "^@loyal-labs/smart-account-vaults$":
      "<rootDir>/../../packages/smart-account-vaults/src/index",
    "^@loyal-labs/solana-rpc$": "<rootDir>/../../packages/solana-rpc/src/index",
    "^@loyal-labs/actions$": "<rootDir>/../../packages/loyal-actions/src/index",
    "^@loyal-labs/loyal-smart-accounts$":
      "<rootDir>/../../packages/loyal-smart-accounts/src/index",
    "^@loyal-labs/loyal-smart-accounts-core$":
      "<rootDir>/../../packages/loyal-smart-accounts-core/src/index",
    "^@loyal-labs/loyal-smart-accounts-core/internal$":
      "<rootDir>/../../packages/loyal-smart-accounts-core/src/internal/index",
    "^@loyal-labs/solana-wallet$":
      "<rootDir>/../../packages/solana-wallet/src/index",
    "^@loyal-labs/solana-instruction-decoder$":
      "<rootDir>/../../packages/solana-instruction-decoder/src/index",
    "^(\\.{1,2}/.*)\\.js$": "$1",
    "^expo-constants$": "<rootDir>/test/expo-constants.js",
    "^expo-seed-vault$": "<rootDir>/modules/expo-seed-vault/src/index",
    "^expo-synced-keychain$": "<rootDir>/modules/expo-synced-keychain/src/index",
    "^react-native-mmkv$": "<rootDir>/test/react-native-mmkv.js",
    "^react-native$": "<rootDir>/test/react-native.js",
    "\\.(png|jpg|jpeg|gif|webp|svg)$": "<rootDir>/test/fileMock.js",
  },
  transform: {
    "^.+\\.tsx?$": "ts-jest",
    "node_modules/@noble/.+\\.js$": "ts-jest",
  },
  transformIgnorePatterns: [
    "node_modules/(?!(@noble/ciphers|@noble/hashes)/)",
  ],
};
