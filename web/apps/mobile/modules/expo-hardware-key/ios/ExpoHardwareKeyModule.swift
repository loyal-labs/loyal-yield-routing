import CryptoKit
import ExpoModulesCore
import LocalAuthentication
import Security

public class ExpoHardwareKeyModule: Module {
  private let algorithm: SecKeyAlgorithm = .eciesEncryptionCofactorVariableIVX963SHA256AESGCM
  private let tagPrefix = "app.askloyal.hardware-key."

  public func definition() -> ModuleDefinition {
    Name("ExpoHardwareKey")

    Function("isSupported") { () -> Bool in
      SecureEnclave.isAvailable
    }

    AsyncFunction("ensureKey") { (alias: String, auth: String) in
      if try self.findKey(alias: alias, context: nil) == nil {
        try self.createKey(alias: alias, auth: auth)
      }
    }

    AsyncFunction("replaceKey") { (alias: String, auth: String) in
      try self.deleteKey(alias: alias)
      try self.createKey(alias: alias, auth: auth)
    }

    AsyncFunction("wrap") { (alias: String, plaintext: String, _: String) -> String in
      guard let key = try self.findKey(alias: alias, context: nil) else {
        throw HardwareKeyInvalidatedException()
      }
      guard let publicKey = SecKeyCopyPublicKey(key) else {
        throw HardwareKeyFailedException("the key has no public half")
      }
      var error: Unmanaged<CFError>?
      guard
        let sealed = SecKeyCreateEncryptedData(
          publicKey, self.algorithm, Data(plaintext.utf8) as CFData, &error) as Data?
      else {
        throw self.exception(from: error)
      }
      return sealed.base64EncodedString()
    }

    AsyncFunction("unwrap") { (alias: String, wrapped: String, prompt: String) -> String in
      guard let sealed = Data(base64Encoded: wrapped) else {
        throw HardwareKeyFailedException("the wrapped value is not base64")
      }
      let context = LAContext()
      context.localizedReason = prompt
      guard let key = try self.findKey(alias: alias, context: context) else {
        throw HardwareKeyInvalidatedException()
      }
      var error: Unmanaged<CFError>?
      guard
        let opened = SecKeyCreateDecryptedData(key, self.algorithm, sealed as CFData, &error)
          as Data?
      else {
        throw self.exception(from: error)
      }
      guard let plaintext = String(data: opened, encoding: .utf8) else {
        throw HardwareKeyFailedException("the unwrapped value is not UTF-8")
      }
      return plaintext
    }

    AsyncFunction("deleteKey") { (alias: String) in
      try self.deleteKey(alias: alias)
    }
  }

  private func tag(_ alias: String) -> Data {
    Data((tagPrefix + alias).utf8)
  }

  private func createKey(alias: String, auth: String) throws {
    var flags: SecAccessControlCreateFlags = [.privateKeyUsage]
    if auth == "biometry" {
      flags.insert(.biometryCurrentSet)
    }
    var error: Unmanaged<CFError>?
    guard
      let access = SecAccessControlCreateWithFlags(
        kCFAllocatorDefault, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, flags, &error)
    else {
      throw exception(from: error)
    }
    let attributes: [String: Any] = [
      kSecAttrKeyType as String: kSecAttrKeyTypeECSECPrimeRandom,
      kSecAttrKeySizeInBits as String: 256,
      kSecAttrTokenID as String: kSecAttrTokenIDSecureEnclave,
      kSecPrivateKeyAttrs as String: [
        kSecAttrIsPermanent as String: true,
        kSecAttrApplicationTag as String: tag(alias),
        kSecAttrAccessControl as String: access,
      ],
    ]
    guard SecKeyCreateRandomKey(attributes as CFDictionary, &error) != nil else {
      throw exception(from: error)
    }
  }

  private func findKey(alias: String, context: LAContext?) throws -> SecKey? {
    var query: [String: Any] = [
      kSecClass as String: kSecClassKey,
      kSecAttrApplicationTag as String: tag(alias),
      kSecAttrKeyType as String: kSecAttrKeyTypeECSECPrimeRandom,
      kSecReturnRef as String: true,
    ]
    if let context {
      query[kSecUseAuthenticationContext as String] = context
    }
    var item: CFTypeRef?
    let status = SecItemCopyMatching(query as CFDictionary, &item)
    if status == errSecItemNotFound {
      return nil
    }
    guard status == errSecSuccess, let item, CFGetTypeID(item) == SecKeyGetTypeID() else {
      throw HardwareKeyFailedException("SecItemCopyMatching failed with status \(status)")
    }
    return (item as! SecKey)
  }

  private func deleteKey(alias: String) throws {
    let query: [String: Any] = [
      kSecClass as String: kSecClassKey,
      kSecAttrApplicationTag as String: tag(alias),
    ]
    let status = SecItemDelete(query as CFDictionary)
    guard status == errSecSuccess || status == errSecItemNotFound else {
      throw HardwareKeyFailedException("SecItemDelete failed with status \(status)")
    }
  }

  private func exception(from error: Unmanaged<CFError>?) -> Exception {
    guard let cfError = error?.takeRetainedValue() else {
      return HardwareKeyFailedException("unknown Security framework error")
    }
    let nsError = cfError as Error as NSError
    if nsError.domain == LAError.errorDomain {
      switch LAError.Code(rawValue: nsError.code) {
      case .userCancel, .appCancel, .systemCancel, .userFallback:
        return HardwareKeyCanceledException()
      case .biometryNotEnrolled, .biometryNotAvailable:
        return HardwareKeyInvalidatedException()
      default:
        return HardwareKeyFailedException(nsError.localizedDescription)
      }
    }
    switch OSStatus(nsError.code) {
    case errSecUserCanceled:
      return HardwareKeyCanceledException()
    case errSecItemNotFound:
      return HardwareKeyInvalidatedException()
    default:
      return HardwareKeyFailedException(nsError.localizedDescription)
    }
  }
}

internal final class HardwareKeyCanceledException: Exception {
  override var code: String { "ERR_HARDWARE_KEY_CANCELED" }
  override var reason: String { "Authentication was canceled" }
}

internal final class HardwareKeyInvalidatedException: Exception {
  override var code: String { "ERR_HARDWARE_KEY_INVALIDATED" }
  override var reason: String { "The hardware key is gone or no longer usable" }
}

internal final class HardwareKeyFailedException: GenericException<String> {
  override var code: String { "ERR_HARDWARE_KEY_FAILED" }
  override var reason: String { "Hardware key operation failed: \(param)" }
}
