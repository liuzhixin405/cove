@echo off
title Cove Quick Builder
echo ===================================================
echo               Cove Quick-Build Script              
echo ===================================================
echo.

:: Fetch Git commit hash dynamically if git is installed
set "COMMIT=unknown"
for /f "tokens=*" %%i in ('git rev-parse --short HEAD 2^>nul') do set "COMMIT=%%i"

:: Fetch current UTC date/time in ISO-like format
set "BUILD_TIME="
for /f "tokens=*" %%i in ('powershell -NoProfile -Command "Get-Date -Format 'yyyy-MM-ddTHH:mm:ssZ'" 2^>nul') do set "BUILD_TIME=%%i"
if "%BUILD_TIME%"=="" (
    set "BUILD_TIME=local-build"
)

echo [1/2] Compiling cove.exe for Windows (CGO_ENABLED=0)...
set "CGO_ENABLED=0"
set "GOOS=windows"
set "GOARCH=amd64"

go build -ldflags "-s -w -X main.Version=5.0.0 -X main.BuildTime=%BUILD_TIME% -X main.GitCommit=%COMMIT%" -o cove.exe ./cli/cove

if %ERRORLEVEL% EQU 0 (
    echo.
    echo [2/2] ===================================================
    echo       ✨ SUCCESS: cove.exe successfully compiled! ✨
    echo       Artifact path: .\cove.exe
    echo       Git Commit:    %COMMIT%
    echo       Build Time:    %BUILD_TIME%
    echo ===================================================
) else (
    echo.
    echo ❌ ERROR: Compilation failed! Please check if Go is installed and on your PATH.
)
echo.
pause
