# Vendored Android SMS Gateway

Upstream: https://github.com/capcom6/android-sms-gateway.git
Commit: 89c56358fd171f02a56b3ac4c6e492d681162a4b

This copy is vendored so the icloud-hme repository can build the debug APK without a Git submodule. Generated Android build outputs, local.properties, and google-services.json stay ignored; tools/build-smsgateway-apk.ps1 creates the local placeholder google-services.json when needed.
