# pushme App 签名：在放签名密钥的机器上跑（Windows PowerShell 5.1 起）。
#
#   .\sign.ps1 <pushme-0.1.N-unsigned.apk>
#
# 脚本所在目录要有：apksigner.jar（Android build-tools 里的）、pushme-release.jks、pushme-release.pass（密钥库密码）。
# 输出去掉 -unsigned 的已签名包，打印签名证书摘要和包的 sha256。java 用 $env:JAVA_HOME 的，没设就用 PATH 上的。
param([Parameter(Mandatory = $true)][string]$Unsigned)
$ErrorActionPreference = "Stop"
$here = $PSScriptRoot
$java = if ($env:JAVA_HOME) { Join-Path $env:JAVA_HOME "bin\java.exe" } else { "java" }
$Unsigned = (Resolve-Path $Unsigned).Path
if ($Unsigned -notmatch '-unsigned\.apk$') { throw "文件名要以 -unsigned.apk 结尾：$Unsigned" }
$signed = $Unsigned -replace '-unsigned\.apk$', '.apk'
$jar = Join-Path $here "apksigner.jar"
$ks = Join-Path $here "pushme-release.jks"
$pass = Join-Path $here "pushme-release.pass"

# 原生程序往 stderr 写字时，5.1 在 Stop 模式下会当成异常；这里改看退出码
$ErrorActionPreference = "Continue"
& $java -jar $jar sign --ks $ks --ks-key-alias pushme --ks-pass "file:$pass" --out $signed $Unsigned 2>&1 | ForEach-Object { "$_" }
if ($LASTEXITCODE -ne 0) { throw "签名失败（exit $LASTEXITCODE）" }
& $java -jar $jar verify --print-certs $signed 2>&1 | ForEach-Object { "$_" }
if ($LASTEXITCODE -ne 0) { throw "签名校验失败（exit $LASTEXITCODE）" }
"signed: $signed"
"sha256: " + (Get-FileHash $signed -Algorithm SHA256).Hash.ToLower()
