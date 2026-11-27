@echo off
rem Castlekeep (test networks only): your wallet, connected to a friend's server.
rem The first time it asks for your wallet's name and the server's address. After that
rem it starts the same wallet, which remembers the server.
title Castlekeep wallet
cd /d "%~dp0console"
where go >nul 2>nul || goto nogo
set "DATA=%USERPROFILE%\castlekeep-data"
set "NAMEFILE=%DATA%\join-name.txt"
set "NAME=%~1"
set "SERVER=%~2"
rem Given as arguments ("Join a server.bat" bob castlekeep.example.com): no questions.
if defined NAME if defined SERVER goto first
if exist "%NAMEFILE%" set /p NAME=<"%NAMEFILE%"
rem A server runs the public test networks, so the wallet's file is in the testnets folder.
if defined NAME if exist "%DATA%\testnets\wallet-%NAME%.json" goto again

echo Castlekeep: test networks only. The coins have no value.
echo.
:askname
set /p "NAME=Your wallet's name (letters and digits only, e.g. bob): "
if not defined NAME goto askname
:askserver
set /p "SERVER=Your friend's server address (e.g. castlekeep.example.com): "
if not defined SERVER goto askserver
:first
if not exist "%DATA%" mkdir "%DATA%"
>"%NAMEFILE%" echo %NAME%
go run . wallet -name %NAME% -server %SERVER%
pause
exit /b

:again
echo Starting %NAME%'s wallet. It remembers the server.
echo (For another name, delete %NAMEFILE% and run this again.)
go run . wallet -name %NAME%
pause
exit /b

:nogo
echo Go isn't installed (or isn't on the PATH). Get it from https://go.dev/dl,
echo install it with the default options, then double-click this file again.
pause
exit /b 1
