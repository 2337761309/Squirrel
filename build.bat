@echo off
set VERSION=1.2

echo Building Windows version...
set GOOS=windows
set GOARCH=amd64
go build -o squirrel.exe