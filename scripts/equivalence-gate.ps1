$ErrorActionPreference = 'Stop'

# This is the release gate for a capability-equivalence claim. It intentionally
# fails when the reference checkout or a cgo-capable race toolchain is absent;
# a skipped gate is evidence of missing verification, not a pass.
$equivalenceRoot = Split-Path -Parent $PSScriptRoot
Set-Location $equivalenceRoot

function Invoke-Required([string]$equivalenceLabel, [scriptblock]$equivalenceCommand) {
    Write-Host "== $equivalenceLabel =="
    & $equivalenceCommand
    if ($LASTEXITCODE -ne 0) {
        throw "$equivalenceLabel failed with exit code $LASTEXITCODE"
    }
}

$equivalenceManifestPath = Join-Path $equivalenceRoot 'docs/equivalence-manifest.yaml'
if (-not (Test-Path -LiteralPath $equivalenceManifestPath -PathType Leaf)) {
    throw "equivalence manifest is missing: $equivalenceManifestPath"
}
$equivalenceManifest = Get-Content -LiteralPath $equivalenceManifestPath -Raw -Encoding utf8
if ($equivalenceManifest -notmatch '(?m)^schemaVersion:\s*1\s*$' -or
    $equivalenceManifest -notmatch '(?m)^kind:\s*shutu-dsh-capability-equivalence\s*$') {
    throw 'equivalence manifest has an unsupported schema or kind'
}

# The markdown checklist is the human narrative; the task register is the
# machine-readable release authority. Keep this parser deliberately small and
# dependency-free: it validates the subset of YAML that the gate needs and
# refuses a claim when a required blocker is not done. This prevents a
# claimAllowed flip from bypassing the actual audit backlog.
$equivalenceTaskRegisterPath = Join-Path $equivalenceRoot 'docs/equivalence-task-register.yaml'
if (-not (Test-Path -LiteralPath $equivalenceTaskRegisterPath -PathType Leaf)) {
    throw "equivalence task register is missing: $equivalenceTaskRegisterPath"
}
$equivalenceTaskRegister = Get-Content -LiteralPath $equivalenceTaskRegisterPath -Raw -Encoding utf8
if ($equivalenceManifest -notmatch '(?m)^\s*taskRegister:\s*docs/equivalence-task-register\.yaml\s*$') {
    throw 'equivalence manifest does not point to the machine-readable task register'
}
Invoke-Required 'task register evidence links' {
    powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'equivalence-register-lint.ps1')
}
$equivalenceTaskBlocks = $equivalenceTaskRegister -split '(?m)(?=^  - id:\s*)' |
    Where-Object { $_ -match '(?m)^  - id:\s*\S+' }
$equivalenceIncompleteRequired = @(
    foreach ($equivalenceTaskBlock in $equivalenceTaskBlocks) {
        $equivalenceTaskID = [regex]::Match($equivalenceTaskBlock, '(?m)^  - id:\s*(\S+)').Groups[1].Value
        $equivalenceTaskStatus = [regex]::Match($equivalenceTaskBlock, '(?m)^    status:\s*(\S+)').Groups[1].Value
        $equivalenceTaskRequired = [regex]::Match($equivalenceTaskBlock, '(?m)^    releaseBlocker:\s*(true|false)').Groups[1].Value
        if ($equivalenceTaskRequired -eq 'true' -and $equivalenceTaskStatus -ne 'done') {
            "$equivalenceTaskID=$equivalenceTaskStatus"
        }
    }
)
if ($equivalenceIncompleteRequired.Count -gt 0 -and
    $equivalenceManifest -match '(?m)^claimAllowed:\s*true\s*$') {
    throw "required equivalence tasks are not done: $($equivalenceIncompleteRequired -join ', ')"
}
$equivalenceClaimAllowed = [regex]::Match(
    $equivalenceManifest,
    '(?m)^claimAllowed:\s*(\S+)'
).Groups[1].Value
if ($equivalenceClaimAllowed -notin @('true', 'false')) {
    throw "equivalence manifest has invalid claimAllowed: $equivalenceClaimAllowed"
}
$expectedEquivalenceStatus = if ($equivalenceClaimAllowed -eq 'true') { 'pass' } else { 'fail' }
$equivalenceStatus = [regex]::Match($equivalenceManifest, '(?m)^status:\s*(\S+)').Groups[1].Value
if ($equivalenceStatus -ne $expectedEquivalenceStatus) {
    throw "equivalence manifest status '$equivalenceStatus' disagrees with claimAllowed '$equivalenceClaimAllowed'"
}
$equivalenceReferenceCommit = [regex]::Match(
    $equivalenceManifest,
    '(?m)^  commit:\s*(\S+)'
).Groups[1].Value
if ([string]::IsNullOrWhiteSpace($equivalenceReferenceCommit)) {
    throw 'equivalence manifest is missing the pinned reference commit'
}

$openBlockerMatch = [regex]::Match(
    $equivalenceManifest,
    '(?ms)^openBlockers:\s*\r?\n((?:  - \S+\s*\r?\n)*)'
)
if (-not $openBlockerMatch.Success) {
    throw 'equivalence manifest is missing an openBlockers list'
}
$manifestOpenBlockers = @(
    [regex]::Matches($openBlockerMatch.Groups[1].Value, '(?m)^  - (\S+)') |
        ForEach-Object { $_.Groups[1].Value } | Sort-Object
)
$computedOpenBlockers = @(
    foreach ($entry in $equivalenceIncompleteRequired) {
        ($entry -split '=')[0]
    }
)
$computedOpenBlockers = @($computedOpenBlockers | Sort-Object)
$blockerDifference = Compare-Object -ReferenceObject $manifestOpenBlockers `
    -DifferenceObject $computedOpenBlockers
if ($blockerDifference) {
    $details = ($blockerDifference | ForEach-Object {
        "$($_.SideIndicator.Trim()) $($_.InputObject)"
    }) -join ', '
    throw "manifest openBlockers disagree with required task states: $details"
}

foreach ($agreementDocument in @(
    'docs/dsh-equivalence-tasks.md',
    'docs/dsh-equivalence-status.md',
    'docs/dsh-tool-capability-parity.md'
)) {
    $agreementPath = Join-Path $equivalenceRoot $agreementDocument
    if (-not (Test-Path -LiteralPath $agreementPath -PathType Leaf)) {
        throw "equivalence agreement document is missing: $agreementDocument"
    }
    $agreementText = Get-Content -LiteralPath $agreementPath -Raw -Encoding utf8
    foreach ($blocker in $manifestOpenBlockers) {
        if ($agreementText -notmatch [regex]::Escape($blocker)) {
            throw "$agreementDocument does not disclose open blocker $blocker"
        }
    }
}

$equivalenceReportPath = Join-Path ([IO.Path]::GetTempPath()) (
    'shutu-equivalence-report-' + [guid]::NewGuid().ToString('N') + '.json'
)
try {
    powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'equivalence-report.ps1') -OutputPath $equivalenceReportPath
    if ($LASTEXITCODE -ne 0) {
        throw "equivalence report generation failed with exit code $LASTEXITCODE"
    }
    $equivalenceReport = Get-Content -LiteralPath $equivalenceReportPath -Raw -Encoding utf8 |
        ConvertFrom-Json
    if ($equivalenceReport.schemaVersion -ne 1 -or
        $equivalenceReport.status -ne $equivalenceStatus -or
        $equivalenceReport.claimAllowed -ne ($equivalenceClaimAllowed -eq 'true') -or
        $equivalenceReport.reference.commit -ne $equivalenceReferenceCommit) {
        throw 'generated equivalence report does not match the manifest'
    }
    if ([string]::IsNullOrWhiteSpace($equivalenceReport.subject.commit) -or
        [string]::IsNullOrWhiteSpace($equivalenceReport.verification.timestampUtc)) {
        throw 'generated equivalence report lacks commit or verification timestamp'
    }
    $verificationTimestamp = [DateTimeOffset]::Parse(
        $equivalenceReport.verification.timestampUtc,
        [Globalization.CultureInfo]::InvariantCulture
    )
    if ([DateTimeOffset]::UtcNow - $verificationTimestamp -gt [TimeSpan]::FromMinutes(10)) {
        throw 'generated equivalence report has a stale verification timestamp'
    }
    $reportProfiles = @($equivalenceReport.reference.profiles | Sort-Object)
    $manifestProfileMatch = [regex]::Match(
        $equivalenceManifest,
        '(?ms)^  profiles:\s*\r?\n((?:    - \S+\s*\r?\n)+)'
    )
    if (-not $manifestProfileMatch.Success) {
        throw 'equivalence manifest is missing reference profiles'
    }
    $manifestProfiles = @(
        [regex]::Matches($manifestProfileMatch.Groups[1].Value, '(?m)^    - (\S+)') |
            ForEach-Object { $_.Groups[1].Value } | Sort-Object
    )
    if (Compare-Object -ReferenceObject $manifestProfiles -DifferenceObject $reportProfiles) {
        throw 'generated equivalence report profiles disagree with the manifest'
    }
    $reportOpenBlockers = @($equivalenceReport.openBlockers | Sort-Object)
    if (Compare-Object -ReferenceObject $manifestOpenBlockers -DifferenceObject $reportOpenBlockers) {
        throw 'generated equivalence report blockers disagree with the manifest'
    }
} finally {
    if (Test-Path -LiteralPath $equivalenceReportPath -PathType Leaf) {
        Remove-Item -LiteralPath $equivalenceReportPath -Force
    }
}

if ($equivalenceClaimAllowed -ne 'true') {
    throw 'capability-equivalence claim is not allowed by docs/equivalence-manifest.yaml'
}

$equivalenceUnformatted = @(
    rg --files -g '*.go' -g '!.gomodcache/**' -g '!vendor/**' |
        ForEach-Object { gofmt -l -- $_ }
)
if ($equivalenceUnformatted.Count -ne 0) {
    $equivalenceUnformatted | ForEach-Object { Write-Host $_ }
    throw 'gofmt check failed: format the listed Go files before release'
}

Invoke-Required 'diff check' { git diff --check }
Invoke-Required 'unit and contract tests' { go test -count=1 ./... }
Invoke-Required 'static analysis' { go vet ./... }
Invoke-Required 'build' { go build ./... }

# The release artifact must carry the exact tool inventory that the runtime
# would expose. Export and immediately verify one manifest through the
# production CLI boundary so packaging cannot bypass the digest contract.
$equivalenceCatalogManifest = Join-Path ([IO.Path]::GetTempPath()) (
    'shutu-tool-catalog-' + [guid]::NewGuid().ToString('N') + '.json'
)
try {
    Invoke-Required 'tool catalog manifest export' {
        go run ./cmd/pa --catalog-manifest $equivalenceCatalogManifest
    }
    Invoke-Required 'tool catalog manifest verify' {
        go run ./cmd/pa --verify-catalog-manifest $equivalenceCatalogManifest
    }
} finally {
    if (Test-Path -LiteralPath $equivalenceCatalogManifest -PathType Leaf) {
        Remove-Item -LiteralPath $equivalenceCatalogManifest -Force
    }
}

# The browser projection is a first-class contract surface, not an optional
# packaging check. Keep its test/build/manifest checks in the same release
# gate as the Go runtime so a green backend cannot mask a stale UI contract.
$equivalenceWebRoot = Join-Path $equivalenceRoot 'web'
if (-not (Test-Path -LiteralPath $equivalenceWebRoot -PathType Container)) {
    throw "web contract root does not exist: $equivalenceWebRoot"
}
Push-Location $equivalenceWebRoot
try {
    Invoke-Required 'web tests' { pnpm.cmd test }
    Invoke-Required 'web build' { pnpm.cmd build }
    Invoke-Required 'web manifest verification' { pnpm.cmd verify }
} finally {
    Pop-Location
}

# Check the two non-CGO release targets as well. This catches accidental use
# of host-specific race/OS APIs while keeping the real race gate below strict.
$equivalencePreviousGOOS = $env:GOOS
$equivalencePreviousGOARCH = $env:GOARCH
$equivalencePreviousCgoForCross = $env:CGO_ENABLED
$env:CGO_ENABLED = '0'
try {
    foreach ($equivalenceTarget in @(@('linux', 'amd64'), @('windows', 'amd64'))) {
        $env:GOOS = $equivalenceTarget[0]
        $env:GOARCH = $equivalenceTarget[1]
        Invoke-Required "cross build $($env:GOOS)/$($env:GOARCH)" { go build ./... }
    }
} finally {
    if ($null -eq $equivalencePreviousGOOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $equivalencePreviousGOOS }
    if ($null -eq $equivalencePreviousGOARCH) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $equivalencePreviousGOARCH }
    if ($null -eq $equivalencePreviousCgoForCross) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $equivalencePreviousCgoForCross }
}

# Race testing needs cgo on Go. Do not hide a missing compiler behind a local
# fallback: CI images must provide a real C compiler and CGO_ENABLED=1.
$equivalencePreviousCgo = $env:CGO_ENABLED
$env:CGO_ENABLED = '1'
try {
    Invoke-Required 'race detector' { go test -race -count=1 ./... }
} finally {
    if ($null -eq $equivalencePreviousCgo) {
        Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
    } else {
        $env:CGO_ENABLED = $equivalencePreviousCgo
    }
}

$equivalenceReference = $env:DSH_REFERENCE_ROOT
if ([string]::IsNullOrWhiteSpace($equivalenceReference)) {
    throw 'DSH_REFERENCE_ROOT is required for the reference replay gate'
}
if (-not (Test-Path -LiteralPath $equivalenceReference -PathType Container)) {
    throw "DSH_REFERENCE_ROOT does not exist: $equivalenceReference"
}
$equivalenceReferenceCommit = (& git -c "safe.directory=$equivalenceReference" -C $equivalenceReference rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or
    $equivalenceManifest -notmatch [regex]::Escape("commit: $equivalenceReferenceCommit")) {
    throw "reference commit does not match docs/equivalence-manifest.yaml: $equivalenceReferenceCommit"
}
Invoke-Required 'reference replay contract' { go test -count=1 ./internal/session -run TestCoreTurnReplayMatchesReference }

Write-Host 'DeepSeek Harness capability-equivalence gates passed.'
