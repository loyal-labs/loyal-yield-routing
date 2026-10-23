package expo.modules.hardwarekey

import android.content.pm.PackageManager
import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyInfo
import android.security.keystore.KeyPermanentlyInvalidatedException
import android.security.keystore.KeyProperties
import android.util.Base64
import androidx.biometric.BiometricManager
import androidx.biometric.BiometricPrompt
import androidx.core.content.ContextCompat
import androidx.fragment.app.FragmentActivity
import expo.modules.kotlin.Promise
import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition
import java.security.KeyStore
import java.security.ProviderException
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.SecretKeyFactory
import javax.crypto.spec.GCMParameterSpec

private const val KEYSTORE = "AndroidKeyStore"
private const val ALIAS_PREFIX = "app.askloyal.hardware-key."
private const val TRANSFORMATION = "AES/GCM/NoPadding"
private const val IV_BYTES = 12
private const val TAG_BITS = 128
private const val CANCELED = "ERR_HARDWARE_KEY_CANCELED"
private const val INVALIDATED = "ERR_HARDWARE_KEY_INVALIDATED"
private const val FAILED = "ERR_HARDWARE_KEY_FAILED"

class ExpoHardwareKeyModule : Module() {
  override fun definition() = ModuleDefinition {
    Name("ExpoHardwareKey")

    Function("isSupported") { true }

    AsyncFunction("ensureKey") { alias: String, auth: String ->
      if (!keyStore().containsAlias(ALIAS_PREFIX + alias)) {
        createKey(alias, auth)
      }
    }

    AsyncFunction("replaceKey") { alias: String, auth: String ->
      deleteAlias(alias)
      createKey(alias, auth)
    }

    AsyncFunction("wrap") { alias: String, plaintext: String, prompt: String, promise: Promise ->
      val key = secretKey(alias) ?: return@AsyncFunction promise.reject(INVALIDATED, "no key for $alias", null)
      val cipher = Cipher.getInstance(TRANSFORMATION)
      try {
        cipher.init(Cipher.ENCRYPT_MODE, key)
      } catch (e: KeyPermanentlyInvalidatedException) {
        return@AsyncFunction promise.reject(INVALIDATED, e.message, e)
      }
      authorize(key, cipher, prompt, promise) { authorized ->
        val sealed = authorized.doFinal(plaintext.toByteArray(Charsets.UTF_8))
        Base64.encodeToString(authorized.iv + sealed, Base64.NO_WRAP)
      }
    }

    AsyncFunction("unwrap") { alias: String, wrapped: String, prompt: String, promise: Promise ->
      val bytes = try {
        Base64.decode(wrapped, Base64.NO_WRAP)
      } catch (e: IllegalArgumentException) {
        return@AsyncFunction promise.reject(FAILED, "the wrapped value is not base64", e)
      }
      if (bytes.size <= IV_BYTES) {
        return@AsyncFunction promise.reject(FAILED, "the wrapped value is too short", null)
      }
      val key = secretKey(alias) ?: return@AsyncFunction promise.reject(INVALIDATED, "no key for $alias", null)
      val cipher = Cipher.getInstance(TRANSFORMATION)
      try {
        cipher.init(Cipher.DECRYPT_MODE, key, GCMParameterSpec(TAG_BITS, bytes, 0, IV_BYTES))
      } catch (e: KeyPermanentlyInvalidatedException) {
        return@AsyncFunction promise.reject(INVALIDATED, e.message, e)
      }
      authorize(key, cipher, prompt, promise) { authorized ->
        String(authorized.doFinal(bytes, IV_BYTES, bytes.size - IV_BYTES), Charsets.UTF_8)
      }
    }

    AsyncFunction("deleteKey") { alias: String ->
      deleteAlias(alias)
    }
  }

  private fun keyStore(): KeyStore = KeyStore.getInstance(KEYSTORE).apply { load(null) }

  private fun deleteAlias(alias: String) {
    runCatching { keyStore().deleteEntry(ALIAS_PREFIX + alias) }
  }

  private fun secretKey(alias: String): SecretKey? =
    keyStore().getKey(ALIAS_PREFIX + alias, null) as? SecretKey

  private fun createKey(alias: String, auth: String) {
    val context = appContext.reactContext ?: throw IllegalStateException("no React context")
    val strongBox = Build.VERSION.SDK_INT >= Build.VERSION_CODES.P &&
      context.packageManager.hasSystemFeature(PackageManager.FEATURE_STRONGBOX_KEYSTORE)
    try {
      generateKey(alias, auth, strongBox)
    } catch (e: ProviderException) {
      if (!strongBox) throw e
      generateKey(alias, auth, false)
    }
  }

  private fun generateKey(alias: String, auth: String, strongBox: Boolean) {
    val builder = KeyGenParameterSpec.Builder(
      ALIAS_PREFIX + alias,
      KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
    )
      .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
      .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
      .setKeySize(256)
    if (strongBox && Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
      builder.setIsStrongBoxBacked(true)
    }
    if (auth == "biometry") {
      builder.setUserAuthenticationRequired(true)
      builder.setInvalidatedByBiometricEnrollment(true)
      if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
        builder.setUserAuthenticationParameters(0, KeyProperties.AUTH_BIOMETRIC_STRONG)
      } else {
        @Suppress("DEPRECATION")
        builder.setUserAuthenticationValidityDurationSeconds(-1)
      }
    }
    KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, KEYSTORE).apply {
      init(builder.build())
      generateKey()
    }
  }

  private fun requiresAuthentication(key: SecretKey): Boolean {
    val info = SecretKeyFactory.getInstance(key.algorithm, KEYSTORE)
      .getKeySpec(key, KeyInfo::class.java) as KeyInfo
    return info.isUserAuthenticationRequired
  }

  private fun authorize(
    key: SecretKey,
    cipher: Cipher,
    prompt: String,
    promise: Promise,
    finish: (Cipher) -> String,
  ) {
    if (!requiresAuthentication(key)) {
      settle(promise) { finish(cipher) }
      return
    }
    val activity = appContext.currentActivity as? FragmentActivity
      ?: return promise.reject(FAILED, "no foreground activity for the biometric prompt", null)
    activity.runOnUiThread {
      val callback = object : BiometricPrompt.AuthenticationCallback() {
        override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) {
          val authorized = result.cryptoObject?.cipher
            ?: return promise.reject(FAILED, "the prompt returned no cipher", null)
          settle(promise) { finish(authorized) }
        }

        override fun onAuthenticationError(errorCode: Int, errString: CharSequence) {
          val code = when (errorCode) {
            BiometricPrompt.ERROR_USER_CANCELED,
            BiometricPrompt.ERROR_NEGATIVE_BUTTON,
            BiometricPrompt.ERROR_CANCELED -> CANCELED
            BiometricPrompt.ERROR_NO_BIOMETRICS,
            BiometricPrompt.ERROR_HW_NOT_PRESENT -> INVALIDATED
            else -> FAILED
          }
          promise.reject(code, errString.toString(), null)
        }
      }
      val info = BiometricPrompt.PromptInfo.Builder()
        .setTitle(prompt)
        .setNegativeButtonText("Use PIN")
        .setAllowedAuthenticators(BiometricManager.Authenticators.BIOMETRIC_STRONG)
        .build()
      BiometricPrompt(activity, ContextCompat.getMainExecutor(activity), callback)
        .authenticate(info, BiometricPrompt.CryptoObject(cipher))
    }
  }

  private fun settle(promise: Promise, block: () -> String) {
    try {
      promise.resolve(block())
    } catch (e: Exception) {
      promise.reject(FAILED, e.message, e)
    }
  }
}
