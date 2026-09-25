@echo off
setlocal
title Cove Quick Builder
echo ===================================================
echo               Cove Quick-Build Script
echo ===================================================
echo.
:: Usage: build.bat [version]
::   With no argument the version is the Version default in cli\cove\main.go,
::   so the binary never claims a version the source does not have.

:: Version: first argument, else read from cli\cove\main.go.
:: Only the text between the first pair of double quotes is taken
:: (Version = "11.0.0" // comment  ->  11.0.0), so spacing around "=" and a
:: trailing comment do not end up in the version.
set "VERSION=%~1"
if "%VERSION%"=="" (
    for /f tokens^=2^ delims^=^" %%v in ('findstr /r /c:"^[	 ]*Version = " cli\cove\main.go') do if not defined VERSION set "VERSION=%%v"
)
if "%VERSION%"=="" (
    echo ERROR: could not read Version from cli\cove\main.go; pass it as the first argument.
    exit /b 1
)

:: Fetch Git commit hash dynamically if git is installed
set "COMMIT=unknown"
for /f "tokens=*" %%i in ('git rev-parse --short HEAD 2^>nul') do set "COMMIT=%%i"

:: Fetch current UTC date/time in ISO-like format
set "BUILD_TIME="
for /f "tokens=*" %%i in ('powershell -NoProfile -Command "(Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')" 2^>nul') do set "BUILD_TIME=%%i"
if "%BUILD_TIME%"=="" (
    set "BUILD_TIME=local-build"
)

echo [1/2] Compiling cove.exe %VERSION% for Windows (CGO_ENABLED=0)...
set "CGO_ENABLED=0"
set "GOOS=windows"
set "GOARCH=amd64"

go build -ldflags "-s -w -X main.Version=%VERSION% -X main.BuildTime=%BUILD_TIME% -X main.GitCommit=%COMMIT%" -o cove.exe ./cli/cove
set "RC=%ERRORLEVEL%"

if %RC% EQU 0 (
    echo.
    echo [2/2] ===================================================
    echo       SUCCESS: cove.exe successfully compiled!
    echo       Artifact path: .\cove.exe
    echo       Version:       %VERSION%
    echo       Git Commit:    %COMMIT%
    echo       Build Time:    %BUILD_TIME%
    echo ===================================================
) else (
    echo.
    echo ERROR: Compilation failed! Please check if Go is installed and on your PATH.
)
echo.
:: Keeps a double-clicked window open; set COVE_BUILD_NOPAUSE=1 in scripts.
if not defined COVE_BUILD_NOPAUSE pause
exit /b %RC%
