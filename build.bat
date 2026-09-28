@echo off
chcp 65001 >nul
cd /d "%~dp0"

where go >nul 2>nul
if not %errorlevel%==0 (
    echo [ERROR] Go not found. Please install Go 1.21+ and add it to PATH.
    echo.
    pause
    exit /b 1
)

echo Building kuaiyuepu-crawler.exe ...
cd go
set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64
go build -trimpath -ldflags "-s -w" -o "..\kuaiyuepu-crawler.exe" .
set "BUILD_RESULT=%errorlevel%"
cd ..

if %BUILD_RESULT%==0 (
    echo.
    echo Build OK: kuaiyuepu-crawler.exe
) else (
    echo.
    echo Build FAILED. See the errors above.
)

echo.
pause
exit /b %BUILD_RESULT%
