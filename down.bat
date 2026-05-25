@echo off
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "Get-Process -Name filebrowser -ErrorAction SilentlyContinue | Stop-Process -Force"
echo Filebrowser stopped.
