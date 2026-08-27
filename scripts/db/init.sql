-- 表结构初始化。后续变更请新增 scripts/db/migrations/NNN_描述.sql，
-- 不要修改本文件 —— 否则已部署环境与新建环境会产生结构漂移。

-- ============ 账号绑定 ============
CREATE TABLE IF NOT EXISTS steam_links (
  user_id           BIGINT UNSIGNED NOT NULL COMMENT '本站用户 ID',
  steam_id64        BIGINT UNSIGNED NOT NULL COMMENT 'SteamID64',
  visibility_state  TINYINT NOT NULL DEFAULT 0
                    COMMENT '0=未探测 1=正常 2=资料私密 3=游戏详情私密',
  private_strikes   TINYINT NOT NULL DEFAULT 0 COMMENT '连续探测到私密的次数',
  linked_at         DATETIME NOT NULL,
  last_verified_at  DATETIME DEFAULT NULL,
  unlinked_at       DATETIME DEFAULT NULL COMMENT '软删除标记',
  created_at        DATETIME NOT NULL,
  updated_at        DATETIME NOT NULL,
  -- 生成列：仅未解绑的记录参与唯一约束，允许 Steam 账号解绑后被他人重新绑定
  active_steam_id   BIGINT UNSIGNED
                    GENERATED ALWAYS AS (IF(unlinked_at IS NULL, steam_id64, NULL)) VIRTUAL,
  PRIMARY KEY (user_id),
  UNIQUE KEY uk_active_steam (active_steam_id),
  KEY idx_steam_id (steam_id64)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ============ 全局共享的游戏元数据（不带用户维度）============
CREATE TABLE IF NOT EXISTS apps (
  appid             INT UNSIGNED NOT NULL,
  name              VARCHAR(255) NOT NULL DEFAULT '',
  img_icon_url      VARCHAR(64) NOT NULL DEFAULT '',
  has_achievements  TINYINT NOT NULL DEFAULT -1 COMMENT '-1=未知 0=无成就 1=有成就',
  ach_total         SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  schema_synced_at  DATETIME DEFAULT NULL,
  created_at        DATETIME NOT NULL,
  updated_at        DATETIME NOT NULL,
  PRIMARY KEY (appid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS app_achievements (
  appid         INT UNSIGNED NOT NULL,
  api_name      VARCHAR(128) NOT NULL COMMENT 'Steam 的 apiname，稳定标识',
  display_name  VARCHAR(255) NOT NULL DEFAULT '',
  description   VARCHAR(1024) NOT NULL DEFAULT '',
  icon          VARCHAR(255) NOT NULL DEFAULT '',
  icon_gray     VARCHAR(255) NOT NULL DEFAULT '',
  hidden        TINYINT NOT NULL DEFAULT 0,
  global_pct    DECIMAL(6,3) NOT NULL DEFAULT 0 COMMENT '全球解锁率百分比',
  created_at    DATETIME NOT NULL,
  updated_at    DATETIME NOT NULL,
  PRIMARY KEY (appid, api_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ============ 用户 × 游戏 当前快照 ============
CREATE TABLE IF NOT EXISTS user_games (
  steam_id64            BIGINT UNSIGNED NOT NULL,
  appid                 INT UNSIGNED NOT NULL,
  playtime_forever_min  INT UNSIGNED NOT NULL DEFAULT 0,
  playtime_2weeks_min   INT UNSIGNED NOT NULL DEFAULT 0,
  rtime_last_played     DATETIME DEFAULT NULL,
  ach_unlocked          SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  ach_total             SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  ach_synced_at         DATETIME DEFAULT NULL,
  first_seen_at         DATETIME NOT NULL COMMENT '首次出现在库中，用于识别新购入',
  created_at            DATETIME NOT NULL,
  updated_at            DATETIME NOT NULL,
  PRIMARY KEY (steam_id64, appid),
  KEY idx_playtime (steam_id64, playtime_forever_min DESC),
  KEY idx_ach_sync (ach_synced_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ============ 时序事件流（append-only）============
CREATE TABLE IF NOT EXISTS play_sessions (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  steam_id64    BIGINT UNSIGNED NOT NULL,
  appid         INT UNSIGNED NOT NULL,
  started_at    DATETIME NOT NULL,
  ended_at      DATETIME NOT NULL,
  duration_min  INT UNSIGNED NOT NULL,
  source        TINYINT NOT NULL COMMENT '1=probe 实测 2=reconcile 推断',
  created_at    DATETIME NOT NULL,
  PRIMARY KEY (id),
  -- 防止租约回收导致任务重跑时写入重复会话
  UNIQUE KEY uk_session (steam_id64, appid, started_at),
  KEY idx_user_time (steam_id64, started_at DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS achievement_unlocks (
  steam_id64   BIGINT UNSIGNED NOT NULL,
  appid        INT UNSIGNED NOT NULL,
  api_name     VARCHAR(128) NOT NULL,
  unlocked_at  DATETIME NOT NULL COMMENT '取 Steam 的 unlocktime，精确值',
  created_at   DATETIME NOT NULL,
  PRIMARY KEY (steam_id64, appid, api_name),
  KEY idx_user_time (steam_id64, unlocked_at DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ============ 探针状态（会话状态机的持久化）============
CREATE TABLE IF NOT EXISTS probe_state (
  steam_id64            BIGINT UNSIGNED NOT NULL,
  current_appid         INT UNSIGNED DEFAULT NULL COMMENT 'NULL 表示 Idle',
  session_started_at    DATETIME DEFAULT NULL,
  last_seen_playing_at  DATETIME DEFAULT NULL COMMENT '最后一次观测到在玩，用作 ended_at',
  miss_count            TINYINT NOT NULL DEFAULT 0 COMMENT '去抖计数',
  tier                  TINYINT NOT NULL DEFAULT 3,
  last_probe_at         DATETIME DEFAULT NULL,
  next_probe_at         DATETIME NOT NULL,
  updated_at            DATETIME NOT NULL,
  PRIMARY KEY (steam_id64),
  KEY idx_next_probe (next_probe_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ============ 本地事务表（异步任务与补偿的核心）============
CREATE TABLE IF NOT EXISTS sync_tasks (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  task_type     TINYINT NOT NULL
                COMMENT '1=库同步 2=成就同步 3=schema同步 4=会话结算',
  steam_id64    BIGINT UNSIGNED NOT NULL DEFAULT 0,
  appid         INT UNSIGNED NOT NULL DEFAULT 0,
  payload       JSON DEFAULT NULL COMMENT '会话结算携带 started_at / ended_at',
  priority      TINYINT NOT NULL DEFAULT 5 COMMENT '数值小者优先',
  status        TINYINT NOT NULL DEFAULT 0
                COMMENT '0=待执行 1=执行中 2=成功 3=待重试 4=死信',
  attempts      SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  next_run_at   DATETIME NOT NULL COMMENT '统一调度时间轴，执行中时兼作租约到期时刻',
  last_error    VARCHAR(512) NOT NULL DEFAULT '',
  created_at    DATETIME NOT NULL,
  updated_at    DATETIME NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_task (task_type, steam_id64, appid),
  KEY idx_scan (status, next_run_at, priority)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
