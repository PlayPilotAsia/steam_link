#!/usr/bin/env bash
# 跑全量测试。集成测试需要本地 MySQL 与 Redis 已就绪。
set -euo pipefail

cd "$(dirname "$0")/../.."

go vet ./...
# -p 1 串行执行各包：store / task / collector 的集成测试共用同一个
# MySQL 库并会清表，并行跑会互相清掉对方的数据。
go test -p 1 ./... "$@"
