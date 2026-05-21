#!/bin/bash
set -e
cd /Users/qiankun/code/agent/code_agent/deploy/bin
echo "Downloading Jaeger v1.65.0 for linux/arm64..."
curl -L --progress-bar -o jaeger.tar.gz "https://github.com/jaegertracing/jaeger/releases/download/v1.65.0/jaeger-1.65.0-linux-arm64.tar.gz"
echo "Extracting jaeger-all-in-one..."
tar xzf jaeger.tar.gz
cp jaeger-1.65.0-linux-arm64/jaeger-all-in-one .
rm -rf jaeger.tar.gz jaeger-1.65.0-linux-arm64
file jaeger-all-in-one
ls -lh jaeger-all-in-one
echo "Done!"
