@echo off
title Chatlog Server
cd /d "%~dp0"

REM ============================================
REM  Required environment variables:
REM    CHATLOG_DATA_DIR  - WeChat data directory
REM    CHATLOG_DATA_KEY  - Database decryption key
REM    CHATLOG_WORK_DIR  - Decrypted output directory
REM
REM  Optional:
REM    CHATLOG_IMG_KEY   - Image decryption key
REM    CHATLOG_ADDR      - Listen address (default: 0.0.0.0:5030)
REM
REM  You can set these via:
REM    1. System environment variables
REM    2. A local .env.bat file (gitignored)
REM ============================================

if exist "%~dp0.env.bat" call "%~dp0.env.bat"

if "%CHATLOG_DATA_DIR%"=="" (
    echo ERROR: CHATLOG_DATA_DIR is not set
    echo Please set environment variables or create .env.bat
    pause
    exit /b 1
)
if "%CHATLOG_DATA_KEY%"=="" (
    echo ERROR: CHATLOG_DATA_KEY is not set
    pause
    exit /b 1
)
if "%CHATLOG_WORK_DIR%"=="" (
    echo ERROR: CHATLOG_WORK_DIR is not set
    pause
    exit /b 1
)

echo [Chatlog Server]
echo Data Dir : %CHATLOG_DATA_DIR%
echo Work Dir : %CHATLOG_WORK_DIR%
echo Address  : %CHATLOG_ADDR% (default: 0.0.0.0:5030)
echo.

set "CMD=bin\chatlog.exe server --platform windows --version 4 --data-dir "%CHATLOG_DATA_DIR%" --data-key "%CHATLOG_DATA_KEY%" --work-dir "%CHATLOG_WORK_DIR%" --auto-decrypt"

if not "%CHATLOG_IMG_KEY%"=="" set "CMD=%CMD% --img-key "%CHATLOG_IMG_KEY%""
if not "%CHATLOG_ADDR%"=="" set "CMD=%CMD% --addr "%CHATLOG_ADDR%""

%CMD%

pause
