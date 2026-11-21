@echo off
cd /d "%~dp0"
set GOPROXY=https://goproxy.cn,direct
set GOTOOLCHAIN=auto
python dev.py
pause
