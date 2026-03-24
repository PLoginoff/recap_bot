#!/usr/bin/env bash

set -euo pipefail

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o recap .

if command -v md5sum >/dev/null 2>&1; then
    echo "MD5: $(md5sum recap)"
else
    echo "No checksum utility found"
fi

echo "Creating archive..."
tar -czf recap_x86.tar.gz recap recap.example.yaml
