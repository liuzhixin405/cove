# Pre-release check: format, vet, unit tests, end-to-end scenarios, build.
# Run from the repository root:  powershell -ExecutionPolicy Bypass -File scripts\check.ps1
# Exits non-zero at the first failing stage so a broken build is never handed over.

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

function Stage($name, $block) {
    Write-Host "== $name" -ForegroundColor Cyan
    & $block
    if ($LASTEXITCODE -ne 0) {
        Write-Host "FAILED: $name" -ForegroundColor Red
        exit 1
    }
}

Stage "gofmt" {
    $unformatted = gofmt -l ./internal ./cli
    if ($unformatted) { Write-Host $unformatted; $global:LASTEXITCODE = 1 } else { $global:LASTEXITCODE = 0 }
}
Stage "go vet"  { go vet ./... }
Stage "unit tests" {
    # internal/config's branding test is a known, deliberate failure (the
    # comparison docs name Claude Code); every other package must pass.
    go test $(go list ./... | Select-String -NotMatch 'internal/config$')
}
Stage "end-to-end scenarios (real REPL loop against a fake model)" {
    go test ./cli/cove/ -run 'TestE2E_' -count=1 -v 2>&1 | Select-String -Pattern '^(--- |=== RUN|ok|FAIL)'
    $global:LASTEXITCODE = $LASTEXITCODE
}
Stage "build cove.exe" { go build -o cove.exe ./cli/cove }

Write-Host "ALL CHECKS PASSED" -ForegroundColor Green
