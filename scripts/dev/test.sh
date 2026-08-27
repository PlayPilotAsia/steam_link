#!/usr/bin/env bash
# 跑全量测试。集成测试需要本地 MySQL 与 Redis 已就绪。
set -euo pipefail

cd "$(dirname "$0")/../.."

go vet ./...
go test ./... "$@"
