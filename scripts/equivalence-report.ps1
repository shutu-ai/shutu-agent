param(
    [Parameter(Mandatory = $true)][string]$OutputPath
)

$ErrorActionPreference = 'Stop'
$equivalenceRoot = Split-Path -Parent $PSScriptRoot
Set-Location $equivalenceRoot

$equivalenceManifestPath = Join-Path $equivalenceRoot 'docs/equivalence-manifest.yaml'
$equivalenceRegisterPath = Join-Path $equivalenceRoot 'docs/equivalence-task-register.yaml'
foreach ($requiredFile in @($equivalenceManifestPath, $equivalenceRegisterPath)) {
    if (-not (Test-Path -LiteralPath $requiredFile -PathType Leaf)) {
        throw "equivalence input is missing: $requiredFile"
    }
}

$manifest = Get-Content -LiteralPath $equivalenceManifestPath -Raw -Encoding utf8
$register = Get-Content -LiteralPath $equivalenceRegisterPath -Raw -Encoding utf8

function Get-EquivalenceScalar([string]$Text, [string]$Pattern) {
    $match = [regex]::Match($Text, $Pattern)
    if (-not $match.Success) {
        throw "equivalence manifest is missing $Pattern"
    }
    return $match.Groups[1].Value.Trim()
}

$status = Get-EquivalenceScalar $manifest '(?m)^status:\s*(\S+)\s*$'
$claimAllowedText = Get-EquivalenceScalar $manifest '(?m)^claimAllowed:\s*(\S+)\s*$'
$referenceCommit = Get-EquivalenceScalar $manifest '(?m)^  commit:\s*(\S+)\s*$'
$baseCommit = Get-EquivalenceScalar $manifest '(?m)^  baseCommit:\s*(\S+)\s*$'
$asOf = Get-EquivalenceScalar $manifest '(?m)^asOf:\s*(\S+)\s*$'
if ($claimAllowedText -notin @('true', 'false')) {
    throw "invalid claimAllowed value: $claimAllowedText"
}
$claimAllowed = $claimAllowedText -eq 'true'
$expectedStatus = if ($claimAllowed) { 'pass' } else { 'fail' }
if ($status -ne $expectedStatus) {
    throw "manifest status '$status' disagrees with claimAllowed '$claimAllowedText'"
}

$actualReferenceCommit = (& git rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0) {
    throw 'unable to read the subject worktree commit'
}
$referenceRoot = Get-EquivalenceScalar $manifest '(?m)^  root:\s*(\S+)\s*$'
$resolvedReferenceRoot = Join-Path $equivalenceRoot $referenceRoot
$pinnedReferenceCommit = (& git -c "safe.directory=$resolvedReferenceRoot" -C $resolvedReferenceRoot rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $pinnedReferenceCommit -ne $referenceCommit) {
    throw "reference checkout does not match pinned commit: expected $referenceCommit, got $pinnedReferenceCommit"
}

$taskBlocks = $register -split '(?m)(?=^  - id:\s*)' |
    Where-Object { $_ -match '(?m)^  - id:\s*\S+' }
$incompleteRequired = @(
    foreach ($taskBlock in $taskBlocks) {
        $id = [regex]::Match($taskBlock, '(?m)^  - id:\s*(\S+)').Groups[1].Value
        $taskStatus = [regex]::Match($taskBlock, '(?m)^    status:\s*(\S+)').Groups[1].Value
        $releaseBlocker = [regex]::Match($taskBlock, '(?m)^    releaseBlocker:\s*(\S+)').Groups[1].Value
        if ($releaseBlocker -eq 'true' -and $taskStatus -ne 'done') {
            $id
        }
    }
)
$profiles = @(
    foreach ($line in ($manifest -split "`n")) {
        if ($line -match '(?m)^\s{4}-\s*(dsh-base|web|headless)\s*$') {
            $Matches[1]
        }
    }
)
if ($profiles.Count -eq 0) {
    throw 'manifest does not declare reference profiles'
}

$verificationTimestamp = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ss.fffZ')
$report = [ordered]@{
    schemaVersion = 1
    kind = 'shutu-dsh-capability-equivalence-report'
    status = $status
    claimAllowed = $claimAllowed
    asOf = $asOf
    verification = [ordered]@{
        timestampUtc = $verificationTimestamp
        generator = 'scripts/equivalence-report.ps1'
    }
    reference = [ordered]@{
        commit = $referenceCommit
        profiles = $profiles
    }
    subject = [ordered]@{
        commit = $actualReferenceCommit
        baseCommit = $baseCommit
        worktreeState = Get-EquivalenceScalar $manifest '(?m)^  worktreeState:\s*(\S+)\s*$'
    }
    openBlockers = $incompleteRequired
}

$outputDirectory = Split-Path -Parent $OutputPath
if ($outputDirectory) {
    New-Item -Path $outputDirectory -ItemType Directory -Force | Out-Null
}
$report | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $OutputPath -Encoding utf8
Write-Host "equivalence report written: $OutputPath"
