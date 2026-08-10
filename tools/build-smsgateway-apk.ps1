param(
  [string]$Output = "build\smsgateway-debug.apk"
)

$ErrorActionPreference = "Stop"
$root = Resolve-Path (Join-Path $PSScriptRoot "..")
$javaHome = Resolve-Path (Join-Path $root ".tools\android\jdk17\jdk-*")
$sdkRoot = Resolve-Path (Join-Path $root ".tools\android\sdk")

$env:JAVA_HOME = $javaHome.Path
$env:ANDROID_SDK_ROOT = $sdkRoot.Path
$env:ANDROID_HOME = $sdkRoot.Path
$env:PATH = "$($javaHome.Path)\bin;$($sdkRoot.Path)\platform-tools;$env:PATH"

$project = Join-Path $root "tools\android-sms-gateway"
$localProps = Join-Path $project "local.properties"
Set-Content -Encoding ASCII -Path $localProps -Value ("sdk.dir=" + ($sdkRoot.Path -replace '\\','\\'))

$googleServices = Join-Path $project "app\google-services.json"
if (-not (Test-Path -LiteralPath $googleServices)) {
  @'
{
  "project_info": {
    "project_number": "1",
    "project_id": "local-smsgateway",
    "storage_bucket": "local-smsgateway.appspot.com"
  },
  "client": [
    {
      "client_info": {
        "mobilesdk_app_id": "1:1:android:1",
        "android_client_info": {
          "package_name": "me.capcom.smsgateway"
        }
      },
      "api_key": [
        {
          "current_key": "AIzaSyDUMMY"
        }
      ],
      "oauth_client": [],
      "services": {
        "appinvite_service": {
          "other_platform_oauth_client": []
        }
      }
    }
  ],
  "configuration_version": "1"
}
'@ | Set-Content -Encoding UTF8 -Path $googleServices
}

Push-Location $project
try {
  .\gradlew.bat --no-daemon assembleDebug
} finally {
  Pop-Location
}

$apk = Join-Path $project "app\build\outputs\apk\debug\app-debug.apk"
$dest = Join-Path $root $Output
New-Item -ItemType Directory -Force -Path (Split-Path $dest) | Out-Null
Copy-Item -LiteralPath $apk -Destination $dest -Force
Write-Host "APK copied to $dest"
