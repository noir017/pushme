package io.github.noir017.pushme

import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.security.keystore.StrongBoxUnavailableException
import android.util.Base64
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.MessageDigest
import java.security.PrivateKey
import java.security.Signature
import java.security.spec.ECGenParameterSpec

/**
 * 审批用的本机密钥：EC P-256，放在系统密钥库（有安全芯片就放安全芯片），私钥导不出来。
 *
 * - 每次签名都要当场验证（指纹或锁屏 PIN），且只有手机解锁时才能用：远程点屏幕、锁屏状态下都签不了；
 * - 新录入指纹不让密钥作废（省得重新配对）；删掉锁屏密码会让系统作废它，那时要重新生成、重新配对；
 * - 公钥（PEM）由人自己装到发起审批的机器上，指纹两边核对。
 */
object Approver {
    private const val STORE = "AndroidKeyStore"
    private const val ALIAS = "pushme-approver"
    const val ALGORITHM = "SHA256withECDSA"

    private fun keyStore(): KeyStore = KeyStore.getInstance(STORE).apply { load(null) }

    fun hasKey(): Boolean = keyStore().containsAlias(ALIAS)

    /** 生成新密钥（已有的会被替换，之前的配对随之失效）。返回是否放进了安全芯片。没设锁屏密码时抛异常。 */
    fun generate(): Boolean {
        keyStore().deleteEntry(ALIAS)
        return try {
            generate(strongBox = true)
            true
        } catch (e: StrongBoxUnavailableException) {
            generate(strongBox = false)
            false
        }
    }

    private fun generate(strongBox: Boolean) {
        val spec = KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)
            .setUserAuthenticationRequired(true)
            // 0 秒：每次签名都要当场验证一次；指纹或锁屏 PIN 都行
            .setUserAuthenticationParameters(0, KeyProperties.AUTH_BIOMETRIC_STRONG or KeyProperties.AUTH_DEVICE_CREDENTIAL)
            .setUnlockedDeviceRequired(true)
            .setInvalidatedByBiometricEnrollment(false)
            .setIsStrongBoxBacked(strongBox)
            .build()
        KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, STORE).apply { initialize(spec) }.generateKeyPair()
    }

    /** 公钥的 SubjectPublicKeyInfo DER；没有密钥为 null。 */
    fun publicKeyDer(): ByteArray? = keyStore().getCertificate(ALIAS)?.publicKey?.encoded

    fun publicKeyPem(): String? = publicKeyDer()?.let { der ->
        val b64 = Base64.encodeToString(der, Base64.NO_WRAP).chunked(64).joinToString("\n")
        "-----BEGIN PUBLIC KEY-----\n$b64\n-----END PUBLIC KEY-----\n"
    }

    /** 与 `pushme fingerprint` 一致：DER 的 SHA-256 前 8 字节，十六进制每 4 位一组。 */
    fun fingerprint(): String? = publicKeyDer()?.let { der ->
        val hex = MessageDigest.getInstance("SHA-256").digest(der).take(8).joinToString("") { "%02x".format(it) }
        hex.chunked(4).joinToString(" ")
    }

    /** 准备好一个签名器交给系统验证框（CryptoObject）；验证通过后才能 sign。密钥被系统作废时抛异常。 */
    fun signer(): Signature {
        val key = keyStore().getKey(ALIAS, null) as? PrivateKey ?: throw IllegalStateException("还没有生成密钥")
        return Signature.getInstance(ALGORITHM).apply { initSign(key) }
    }

    /**
     * 要签名的内容，与 pushme 的 approval.Payload 逐字节一致（7 行，"\n" 分隔，末尾不带换行）：
     * pushme-approval-v1 / id / challenge / approve / expires / sha256hex(title) / sha256hex(text)。
     */
    fun payload(a: Approval): ByteArray = listOf(
        "pushme-approval-v1",
        a.id,
        a.challenge,
        "approve",
        a.expires.toString(),
        sha256Hex(a.title),
        sha256Hex(a.text),
    ).joinToString("\n").toByteArray(Charsets.UTF_8)

    private fun sha256Hex(s: String): String =
        MessageDigest.getInstance("SHA-256").digest(s.toByteArray(Charsets.UTF_8)).joinToString("") { "%02x".format(it) }
}
