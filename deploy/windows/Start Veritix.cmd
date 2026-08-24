@echo off
rem Start Veritix and open it in your browser.
rem
rem This is what a Windows desktop double-clicks. The product is a web
rem interface, so the two things that have to happen are that the server
rem starts and that a browser arrives at it, and neither of them should
rem require typing anything.
rem
rem The window that opens is the server. Closing it, or pressing Ctrl-C in
rem it, stops Veritix. Nothing leaves this machine either way.

cd /d "%~dp0"
echo Starting Veritix. This window is the server: close it to stop.
echo.
veritix.exe serve --open
if errorlevel 1 (
  echo.
  echo Veritix stopped with an error. The lines above say why.
  pause
)
