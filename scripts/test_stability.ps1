param(
    [switch]$SkipRace,
    [int]$TUICount = 20,
    [int]$E2ECount = 80,
    [int]$EngineCount = 15
)

$ErrorActionPreference = "Stop"
$script:results = New-Object System.Collections.Generic.List[object]

function Add-Result {
    param(
        [string]$Name,
        [string]$Status,
        [double]$Seconds,
        [string]$Detail = ""
    )

    $script:results.Add([pscustomobject]@{
        Step    = $Name
        Status  = $Status
        Seconds = [math]::Round($Seconds, 2)
        Detail  = $Detail
    }) | Out-Null
}

function Run-Step {
    param(
        [string]$Name,
        [string]$Command
    )

    Write-Host ""
    Write-Host "==> $Name" -ForegroundColor Cyan
    Write-Host "    $Command"
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        & powershell -NoProfile -Command $Command
        if ($LASTEXITCODE -ne 0) {
            throw "Exit code $LASTEXITCODE"
        }
        $sw.Stop()
        Add-Result -Name $Name -Status "PASS" -Seconds $sw.Elapsed.TotalSeconds
    }
    catch {
        $sw.Stop()
        Add-Result -Name $Name -Status "FAIL" -Seconds $sw.Elapsed.TotalSeconds -Detail $_.Exception.Message
        throw "Step failed: $Name - $($_.Exception.Message)"
    }
}

function Has-Gcc {
    $gcc = Get-Command gcc -ErrorAction SilentlyContinue
    return $null -ne $gcc
}

$root = Resolve-Path (Join-Path $PSScriptRoot "..")
Set-Location $root

Write-Host "Running stability suite in $root" -ForegroundColor Green

$hasGcc = Has-Gcc
if ($hasGcc) {
    $env:CGO_ENABLED = "1"
    Write-Host "Detected gcc, set CGO_ENABLED=1 for this run." -ForegroundColor DarkGray
}

try {
    if (-not $SkipRace) {
        if ($hasGcc) {
            Run-Step -Name "Race check (internal/tui)" -Command "go test -race ./internal/tui"
        }
        else {
            Write-Host ""
            Write-Host "==> Race check skipped (gcc not found in PATH)" -ForegroundColor Yellow
            Add-Result -Name "Race check (internal/tui)" -Status "SKIP" -Seconds 0 -Detail "gcc not found"
        }
    }
    else {
        Add-Result -Name "Race check (internal/tui)" -Status "SKIP" -Seconds 0 -Detail "explicitly skipped by -SkipRace"
    }

    Run-Step -Name "TUI randomized repeat" -Command "go test ./internal/tui -shuffle=on -count=$TUICount"
    Run-Step -Name "TUI E2E high repeat" -Command "go test ./internal/tui -run 'TestProgramE2E_' -count=$E2ECount"
    Run-Step -Name "Engine repeat" -Command "go test ./internal/engine -count=$EngineCount"
    Run-Step -Name "Full repo randomized" -Command "go test ./... -shuffle=on"
}
finally {
    Write-Host ""
    Write-Host "=== Stability Summary ===" -ForegroundColor Magenta

    foreach ($row in $script:results) {
        $color = "Gray"
        if ($row.Status -eq "PASS") { $color = "Green" }
        elseif ($row.Status -eq "FAIL") { $color = "Red" }
        elseif ($row.Status -eq "SKIP") { $color = "Yellow" }

        $line = "{0,-28} {1,-5} {2,7}s" -f $row.Step, $row.Status, $row.Seconds
        if ($row.Detail) {
            $line += "  ($($row.Detail))"
        }
        Write-Host $line -ForegroundColor $color
    }

    $raceRow = $script:results | Where-Object { $_.Step -eq "Race check (internal/tui)" } | Select-Object -First 1
    if ($raceRow) {
        Write-Host ""
        Write-Host "Race status: $($raceRow.Status)" -ForegroundColor Cyan
    }
}

$failed = $script:results | Where-Object { $_.Status -eq "FAIL" }
if ($failed) {
    throw "Stability suite failed."
}

Write-Host ""
Write-Host "Stability suite passed." -ForegroundColor Green
