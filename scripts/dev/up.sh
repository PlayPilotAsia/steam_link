#!/usr/bin/env bash
# 起本地依赖并初始化数据库。可重复执行。
#
# 默认复用本机常驻的开发容器（dev-mysql / dev-redis），因为 3306/6379
# 已被它们占用。若这两个容器不存在，脚本会退回到本仓库的 docker-compose.yml。
set -euo pipefail

cd "$(dirname "$0")/../.."

MYSQL_CONTAINER="${MYSQL_CONTAINER:-dev-mysql}"
MYSQL_ROOT_PASSWORD="${MYSQL_ROOT_PASSWORD:-localdev-root}"
MYSQL_DATABASE="${MYSQL_DATABASE:-steamlink}"

if ! docker inspect "$MYSQL_CONTAINER" >/dev/null 2>&1; then
  echo "==> 未找到容器 $MYSQL_CONTAINER，改用 docker compose 起本仓库依赖"
  docker compose up -d --wait
  MYSQL_CONTAINER="$(docker compose ps -q mysql)"
  MYSQL_ROOT_PASSWORD=root
fi

# --default-character-set=utf8mb4 不能省：mysql 客户端默认按 locale 推导字符集，
# 而官方镜像里 LANG 是空的，会回落成 latin1，把 DDL 里的中文注释按 cp1252
# 重新编码后写进元数据（表面现象是 SHOW CREATE TABLE 里的 COMMENT 变成乱码）。
mysql_exec() {
  docker exec -i "$MYSQL_CONTAINER" \
    mysql --default-character-set=utf8mb4 -uroot -p"$MYSQL_ROOT_PASSWORD" "$@" \
    2>&1 | grep -v '^mysql: \[Warning\]' || true
}

echo "==> 确保数据库 $MYSQL_DATABASE 存在"
mysql_exec -e "CREATE DATABASE IF NOT EXISTS \`$MYSQL_DATABASE\` \
  CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"

echo "==> 应用 scripts/db/init.sql"
mysql_exec "$MYSQL_DATABASE" < scripts/db/init.sql

echo "==> 应用增量脚本"
for f in scripts/db/migrations/*.sql; do
  [ -e "$f" ] || continue
  echo "    $f"
  mysql_exec "$MYSQL_DATABASE" < "$f"
done

echo "==> 完成"
