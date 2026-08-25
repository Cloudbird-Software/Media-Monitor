// package model — canonical data types shared across collectors, live
// monitors, mobile automation, and the MCP/CLI surface.
//
// Field-completeness contract for user objects (see AGENTS.md):
//
//	uid, sec_uid (MS4 form), short_id, nickname, avatar_url, signature,
//	ip_label, gender, follower_count, following_count, aweme_count,
//	total_favorited.
package model

// UserProfile is the normalized author/actor record. All fields are
// best-effort: collectors fill what the platform exposes; canaries assert
// the mandatory subset per contract.
type UserProfile struct {
	UID            string         `json:"uid"`        // numeric user id
	SecUID         string         `json:"sec_uid"`    // MS4wLjAB... opaque id
	ShortID        string         `json:"short_id"`   // dy short id when present
	Nickname       string         `json:"nickname"`   // display name
	AvatarURL      string         `json:"avatar_url"` // avatar uri list (first)
	Signature      string         `json:"signature"`  // profile bio
	IPLabel        string         `json:"ip_label"`   // region label, e.g. "陕西"
	Gender         int            `json:"gender"`     // 0 unknown, 1 male, 2 female
	FollowerCount  int64          `json:"follower_count"`
	FollowingCount int64          `json:"following_count"`
	AwemeCount     int64          `json:"aweme_count"`
	TotalFavorited int64          `json:"total_favorited"`
	Extra          map[string]any `json:"extra,omitempty"`
}

// Item is a search/feed result (video or image post).
type Item struct {
	ID         string         `json:"id"`         // aweme id / note id
	MediaType  string         `json:"media_type"` // "video" | "image" | "unknown"
	Desc       string         `json:"desc"`       // caption text
	CoverURL   string         `json:"cover_url"`
	Author     UserProfile    `json:"author"`
	CreateTime int64          `json:"create_time"` // unix seconds
	Stats      ItemStats      `json:"stats"`
	Extra      map[string]any `json:"extra,omitempty"`
}

// ItemStats aggregates engagement counts of an item.
type ItemStats struct {
	Digg    int64 `json:"digg"`
	Comment int64 `json:"comment"`
	Collect int64 `json:"collect"`
	Share   int64 `json:"share"`
}

// Comment is a top-level comment or reply.
type Comment struct {
	CID        string         `json:"cid"`
	AwemeID    string         `json:"aweme_id"`
	Text       string         `json:"text"`
	CreateTime int64          `json:"create_time"`
	DiggCount  int64          `json:"digg_count"`
	ReplyCount int64          `json:"reply_count"`
	Sticky     bool           `json:"sticky"`
	ReplyToCID string         `json:"reply_to_cid,omitempty"`
	User       UserProfile    `json:"user"`
	Extra      map[string]any `json:"extra,omitempty"`
}

// LiveEvent is one monitored live-room occurrence.
type LiveEvent struct {
	RoomID     string         `json:"room_id"`
	Event      string         `json:"event"` // enter|like|chat|gift|follow|fansclub|rank|room_stat|control
	User       UserProfile    `json:"user"`
	Content    string         `json:"content,omitempty"` // chat text, gift name, ...
	Count      int64          `json:"count,omitempty"`   // like count, viewer count, ...
	Time       int64          `json:"time"`              // unix seconds
	RawSummary map[string]any `json:"raw_summary,omitempty"`
}

// GroupMember is a member of a target group (silent enumeration).
type GroupMember struct {
	UserProfile
	GroupID  string `json:"group_id"`
	JoinedAt int64  `json:"joined_at,omitempty"`
}

// Cursor is the pagination position; opaque to callers, JSON-safe, resumable.
type Cursor struct {
	Page    int64          `json:"page"`
	HasMore bool           `json:"has_more"`
	Source  map[string]any `json:"source,omitempty"`
}

// Task is a unit of collection/monitoring work.
type Task struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"` // search|comments|replies|users|group_members|live_monitor|flow
	Config    JSONMap `json:"config"`
	State     string  `json:"state"` // queued|running|done|failed|cancelled
	Error     string  `json:"error,omitempty"`
	Cursor    Cursor  `json:"cursor,omitempty"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
	Progress  int64   `json:"progress"`
}

// JSONMap is a generic JSON object carrier.
type JSONMap = map[string]any
