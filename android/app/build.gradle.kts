import org.jetbrains.kotlin.gradle.dsl.JvmTarget

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// CI 只出未签名的 release 包（app-release-unsigned.apk）：签名密钥不进 CI，由人在自己的机器上签。
// 理由：仓库谁都可能推代码，CI 能签名就等于谁都能出一个能覆盖安装的新版审批器。版本号由 CI 经环境变量注入。
android {
    namespace = "io.github.noir017.pushme"
    compileSdk = 35

    defaultConfig {
        applicationId = "io.github.noir017.pushme"
        // 审批要「每次签名都验指纹或 PIN」的密钥（setUserAuthenticationParameters），Android 11 起才有
        minSdk = 30
        targetSdk = 35
        versionCode = (System.getenv("VERSION_CODE") ?: "1").toInt()
        versionName = System.getenv("VERSION_NAME") ?: "0.1.0-dev"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    lint {
        // 自用侧载 App：不让 lint 警告卡住构建
        abortOnError = false
        checkReleaseBuilds = false
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(JvmTarget.JVM_17)
    }
}
