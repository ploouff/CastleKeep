@echo off
rem Castlekeep (test networks only): the test chains, the exchange, a watchtower and
rem your wallet, all in this window. Press Ctrl+C here to stop everything.
rem Add options after the name, e.g. from Command Prompt:  "Start Castlekeep.bat" -name alice
title Castlekeep
cd /d "%~dp0console"
where go >nul 2>nul || goto nogo
go run . start %*
pause
exit /b

:nogo
echo Go isn't installed (or isn't on the PATH). Get it from https://go.dev/dl,
echo install it with the default options, then double-click this file again.
pause
exit /b 1
