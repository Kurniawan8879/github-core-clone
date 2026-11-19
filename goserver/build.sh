#!/bin/sh
# Build the static forge binary. Run from goserver/ (or anywhere; the script
# cds to its own directory). The binary MUST stay < 8 MiB; abort + report to
# the coordinator if it passes 7.5 MiB.
set -e
cd "$(dirname "$0")"
GO=~/workspace/tools/go/go/bin/go
CGO_ENABLED=0 $GO build -trimpath -ldflags="-s -w" -o forge .
ls -l forge
