#!/bin/bash

# 设置版本信息
VERSION="1.2"

# 构建Linux版本
echo "Building Linux-amd64 version..."
GOOS=linux GOARCH=amd64 go build -o squirrel