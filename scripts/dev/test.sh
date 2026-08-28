#!/usr/bin/env bash
# 跑全量测试。集成测试需要本地 MySQL 与 Redis 已就绪。
set -euo pipefail

cd "$(dirname "$0")/../.."

go vet ./...
# 可以放心并行：每个测试包用自己的库（见 internal/testsupport），
# 不会再互相清表。
go test ./... "$@"
