param(
    [string]$RegisterPath = (Join-Path (Split-Path -Parent $PSScriptRoot) 'docs/equivalence-task-register.yaml')
)

$ErrorActionPreference = 'Stop'
if (-not (Test-Path -LiteralPath $RegisterPath -PathType Leaf)) {
    throw "equivalence task register is missing: $RegisterPath"
}

$register = Get-Content -LiteralPath $RegisterPath -Raw -Encoding utf8
$blocks = $register -split '(?m)(?=^  - id:\s*)' |
    Where-Object { $_ -match '(?m)^  - id:\s*\S+' }
if ($blocks.Count -eq 0) {
    throw 'equivalence task register contains no tasks'
}

$knownStatuses = @('done', 'partial', 'open', 'blocked')
$seenIDs = @{}
$lintErrors = @()
$doneCount = 0

foreach ($block in $blocks) {
    $id = [regex]::Match($block, '(?m)^  - id:\s*(\S+)').Groups[1].Value
    $status = [regex]::Match($block, '(?m)^    status:\s*(\S+)').Groups[1].Value
    $priority = [regex]::Match($block, '(?m)^    priority:\s*(\S+)').Groups[1].Value
    $objective = [regex]::Match($block, '(?m)^    objective:\s*(.*)$').Groups[1].Value.Trim()
    $implementation = [regex]::Match($block, '(?m)^    implementation:\s*(.*)$').Groups[1].Value.Trim()
    $command = [regex]::Match($block, '(?m)^    command:\s*(.*)$').Groups[1].Value.Trim()
    $evidence = [regex]::Match($block, '(?m)^    evidence:\s*(.*)$').Groups[1].Value.Trim()
    $releaseBlocker = [regex]::Match($block, '(?m)^    releaseBlocker:\s*(\S+)').Groups[1].Value

    if ([string]::IsNullOrWhiteSpace($id)) {
        $lintErrors += 'task is missing id'
        continue
    }
    if ($seenIDs.ContainsKey($id)) {
        $lintErrors += "$id duplicate task id"
    }
    $seenIDs[$id] = $true

    if ($status -notin $knownStatuses) {
        $lintErrors += "$id invalid status '$status'"
    }
    foreach ($field in @(
        @{Name = 'priority'; Value = $priority},
        @{Name = 'objective'; Value = $objective},
        @{Name = 'implementation'; Value = $implementation},
        @{Name = 'command'; Value = $command},
        @{Name = 'releaseBlocker'; Value = $releaseBlocker}
    )) {
        if ([string]::IsNullOrWhiteSpace($field.Value)) {
            $lintErrors += "$id missing $($field.Name)"
        }
    }
    if ($releaseBlocker -notin @('true', 'false')) {
        $lintErrors += "$id invalid releaseBlocker '$releaseBlocker'"
    }
    if ($block -notmatch '(?m)^    acceptance:\s*$' -or $block -notmatch '(?m)^      - \S') {
        $lintErrors += "$id missing acceptance criteria"
    }

    if ($status -ne 'done') {
        continue
    }
    $doneCount++

    foreach ($field in @(
        @{Name = 'implementation'; Value = $implementation},
        @{Name = 'evidence'; Value = $evidence}
    )) {
        foreach ($path in ($field.Value -split '\s*;\s*')) {
            if ([string]::IsNullOrWhiteSpace($path)) {
                $lintErrors += "$id $($field.Name) contains an empty path"
                continue
            }
            $candidate = Join-Path (Split-Path -Parent $PSScriptRoot) $path
            if (-not (Test-Path -LiteralPath $candidate)) {
                $lintErrors += "$id $($field.Name) path does not exist: $path"
            }
        }
    }

    if ($evidence -notmatch '(_test\.go|scripts/|docs/evidence/|docs/equivalence-manifest\.yaml)') {
        $lintErrors += "$id evidence has no executable test, gate, fixture or checked-in replay artifact"
    }
    if ($command -notmatch '^(go test|powershell|pnpm|node)(\s|$)') {
        $lintErrors += "$id acceptance command is not a recognized replay command"
    }
}

if ($lintErrors.Count -gt 0) {
    Write-Host 'equivalence task register evidence audit failed:'
    foreach ($lintError in $lintErrors) {
        Write-Host " - $lintError"
    }
    throw "equivalence task register has $($lintErrors.Count) evidence-linkage error(s)"
}

Write-Host "equivalence task register evidence audit passed: $($seenIDs.Count) tasks, $doneCount done"
