package store

import "time"

// 可见性状态，对应 steam_links.visibility_state
const (
	VisibilityUnknown            int8 = 0
	VisibilityOK                 int8 = 1
	VisibilityProfilePrivate     int8 = 2
	VisibilityGameDetailsPrivate int8 = 3
)

// 会话来源，对应 play_sessions.source
const (
	SourceProbe     int8 = 1 // 探针实测，起止时刻可信
	SourceReconcile int8 = 2 // 每日校准推断，仅时长可信
)

type SteamLink struct {
	UserID          uint64     `gorm:"primaryKey;column:user_id"`
	SteamID         uint64     `gorm:"column:steam_id64"`
	VisibilityState int8       `gorm:"column:visibility_state"`
	PrivateStrikes  int8       `gorm:"column:private_strikes"`
	LinkedAt        time.Time  `gorm:"column:linked_at"`
	LastVerifiedAt  *time.Time `gorm:"column:last_verified_at"`
	UnlinkedAt      *time.Time `gorm:"column:unlinked_at"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// ActiveSteamID 是数据库生成列，只读。GORM 必须标记为 "->" 否则会尝试写入并报错。
	ActiveSteamID *uint64 `gorm:"column:active_steam_id;->"`
}

func (SteamLink) TableName() string { return "steam_links" }

type App struct {
	AppID           uint32     `gorm:"primaryKey;column:appid"`
	Name            string     `gorm:"column:name"`
	ImgIconURL      string     `gorm:"column:img_icon_url"`
	HasAchievements int8       `gorm:"column:has_achievements"` // -1 未知 0 无 1 有
	AchTotal        uint16     `gorm:"column:ach_total"`
	SchemaSyncedAt  *time.Time `gorm:"column:schema_synced_at"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (App) TableName() string { return "apps" }

type AppAchievement struct {
	AppID       uint32  `gorm:"primaryKey;column:appid"`
	APIName     string  `gorm:"primaryKey;column:api_name"`
	DisplayName string  `gorm:"column:display_name"`
	Description string  `gorm:"column:description"`
	Icon        string  `gorm:"column:icon"`
	IconGray    string  `gorm:"column:icon_gray"`
	Hidden      int8    `gorm:"column:hidden"`
	GlobalPct   float64 `gorm:"column:global_pct"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (AppAchievement) TableName() string { return "app_achievements" }

type UserGame struct {
	SteamID            uint64     `gorm:"primaryKey;column:steam_id64"`
	AppID              uint32     `gorm:"primaryKey;column:appid"`
	PlaytimeForeverMin uint32     `gorm:"column:playtime_forever_min"`
	Playtime2WeeksMin  uint32     `gorm:"column:playtime_2weeks_min"`
	RtimeLastPlayed    *time.Time `gorm:"column:rtime_last_played"`
	AchUnlocked        uint16     `gorm:"column:ach_unlocked"`
	AchTotal           uint16     `gorm:"column:ach_total"`
	AchSyncedAt        *time.Time `gorm:"column:ach_synced_at"`
	FirstSeenAt        time.Time  `gorm:"column:first_seen_at"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (UserGame) TableName() string { return "user_games" }

type PlaySession struct {
	ID          uint64    `gorm:"primaryKey;column:id"`
	SteamID     uint64    `gorm:"column:steam_id64"`
	AppID       uint32    `gorm:"column:appid"`
	StartedAt   time.Time `gorm:"column:started_at"`
	EndedAt     time.Time `gorm:"column:ended_at"`
	DurationMin uint32    `gorm:"column:duration_min"`
	Source      int8      `gorm:"column:source"`
	CreatedAt   time.Time
}

func (PlaySession) TableName() string { return "play_sessions" }

type AchievementUnlock struct {
	SteamID    uint64    `gorm:"primaryKey;column:steam_id64"`
	AppID      uint32    `gorm:"primaryKey;column:appid"`
	APIName    string    `gorm:"primaryKey;column:api_name"`
	UnlockedAt time.Time `gorm:"column:unlocked_at"`
	CreatedAt  time.Time
}

func (AchievementUnlock) TableName() string { return "achievement_unlocks" }

type ProbeState struct {
	SteamID           uint64     `gorm:"primaryKey;column:steam_id64"`
	CurrentAppID      *uint32    `gorm:"column:current_appid"` // nil = Idle
	SessionStartedAt  *time.Time `gorm:"column:session_started_at"`
	LastSeenPlayingAt *time.Time `gorm:"column:last_seen_playing_at"`
	MissCount         int8       `gorm:"column:miss_count"`
	Tier              int8       `gorm:"column:tier"`
	LastProbeAt       *time.Time `gorm:"column:last_probe_at"`
	NextProbeAt       time.Time  `gorm:"column:next_probe_at"`
	UpdatedAt         time.Time
}

func (ProbeState) TableName() string { return "probe_state" }

type SyncTask struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	Type      int8      `gorm:"column:task_type"`
	SteamID   uint64    `gorm:"column:steam_id64"`
	AppID     uint32    `gorm:"column:appid"`
	Payload   []byte    `gorm:"column:payload;type:json"`
	Priority  int8      `gorm:"column:priority"`
	Status    int8      `gorm:"column:status"`
	Attempts  uint16    `gorm:"column:attempts"`
	NextRunAt time.Time `gorm:"column:next_run_at"`
	LastError string    `gorm:"column:last_error"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (SyncTask) TableName() string { return "sync_tasks" }
