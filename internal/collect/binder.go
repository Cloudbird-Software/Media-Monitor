// Package collect implements the contract-driven collection engine: every
// platform endpoint, parameter, binding, pagination and signing requirement
// is declared in the adapt/contracts JSON contracts, and this engine executes
// them without any platform-specific code. Platform assemblies (contract
// names, UA/cookie hints, signer hooks) live in internal/platforms.
package collect

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// relSeg is one parsed segment of a record-relative field path. Exactly one
// of key/index/star is meaningful: star selects every element of an array or
// every value of an object, index addresses one element, key addresses one
// object field. A segment parsed from the "key[index]" form carries both key
// and indexed=true (the walker descends the key first, then indexes into the
// resulting array; a negative index counts from the end, "[-1]" = last).
type relSeg struct {
	key     string
	index   int
	star    bool
	indexed bool // "key[0]" / "key[-1]" form (index is meaningful, may be < 0)
}

// parseRel parses a record-relative binding path. Accepted forms: "uid",
// "user.uid", "a[0].b", "a[-1].b", "a[].b", "a[*].b", "*.b", "$.uid",
// "$.data[].uid". "[]" and "[*]" are array wildcards (same semantics as the
// contracts JSONPath extension). Negative indexes count from the end.
func parseRel(raw string) ([]relSeg, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimPrefix(s, ".")
	if s == "" {
		return nil, nil
	}
	var out []relSeg
	for _, part := range strings.Split(s, ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "*" {
			out = append(out, relSeg{star: true})
			continue
		}
		key := part
		if i := strings.IndexByte(part, '['); i >= 0 && strings.HasSuffix(part, "]") {
			key = part[:i]
			inner := part[i+1 : len(part)-1]
			switch inner {
			case "", "*":
				out = append(out, relSeg{key: key, index: -1}, relSeg{star: true})
				continue
			default:
				n, err := strconv.Atoi(inner)
				if err != nil {
					return nil, fmt.Errorf("field path %q: bad index %q", raw, part)
				}
				out = append(out, relSeg{key: key, index: n, indexed: true})
				continue
			}
		}
		out = append(out, relSeg{key: key, index: -1})
	}
	return out, nil
}

// dropLeadingStars removes leading wildcard segments: after a binding prefix
// is stripped, a leading star is the element selector ("$.data[].x" against a
// record reached via binding "$.data" leaves "[*] x"), and the record is
// already one element — the star selects nothing further.
func dropLeadingStars(segs []relSeg) []relSeg {
	for len(segs) > 0 && segs[0].star {
		segs = segs[1:]
	}
	return segs
}

// stripBinding removes the binding root prefix from a parsed field path.
// Binding "$.comments" with field "$.comments[].user.uid" yields the
// record-relative path "user.uid".
func stripBinding(rel, bind []relSeg) []relSeg {
	if len(bind) == 0 {
		return dropLeadingStars(rel)
	}
	i := 0
	for i < len(rel) && i < len(bind) && rel[i] == bind[i] {
		i++
	}
	return dropLeadingStars(rel[i:])
}

// resolveSegs walks rel through rec and returns the first reachable value
// (nil when the path is absent). Wildcards return the first leaf in
// encounter order; object wildcards collect map values in Go map iteration
// order (undetermined when multiple candidate leaves exist).
func resolveSegs(rec map[string]any, rel []relSeg) any {
	rel = dropLeadingStars(rel)
	if len(rel) == 0 {
		return rec
	}
	cur := []any{rec}
	for _, s := range rel {
		var next []any
		switch {
		case s.star:
			for _, v := range cur {
				switch t := v.(type) {
				case []any:
					next = append(next, t...)
				case map[string]any:
					for _, vv := range t {
						next = append(next, vv)
					}
				}
			}
		case s.indexed:
			// "key[0]" / "key[-1]": descend the key first (when present),
			// then index into the resulting array. A negative index counts
			// from the end. (Report G4: the old index branch never descended
			// the key, so "avatar_thumb.url_list[0]" always missed on maps.)
			for _, v := range cur {
				if s.key != "" {
					m, ok := v.(map[string]any)
					if !ok {
						continue
					}
					vv, ok := m[s.key]
					if !ok {
						continue
					}
					v = vv
				}
				arr, ok := v.([]any)
				if !ok {
					continue
				}
				i := s.index
				if i < 0 {
					i += len(arr)
				}
				if i >= 0 && i < len(arr) {
					next = append(next, arr[i])
				}
			}
		case s.index >= 0:
			for _, v := range cur {
				if arr, ok := v.([]any); ok && s.index < len(arr) {
					next = append(next, arr[s.index])
				}
			}
		default:
			for _, v := range cur {
				if m, ok := v.(map[string]any); ok {
					if vv, ok := m[s.key]; ok {
						next = append(next, vv)
					}
				}
			}
		}
		cur = next
		if len(cur) == 0 {
			return nil
		}
	}
	return cur[0]
}

// bindingSegs parses the contract's primary list binding into relative
// segments (used to strip the root prefix from declared field paths).
func bindingSegs(c *contracts.Contract) []relSeg {
	_, raw := mainBindingRaw(c)
	if raw == "" {
		return nil
	}
	segs, err := parseRel(raw)
	if err != nil {
		return nil
	}
	return segs
}

// resolveValue resolves a field value: a contract-declared path when given,
// otherwise the first hit among the default candidate paths.
func resolveValue(c *contracts.Contract, declared string, rec map[string]any, defaults []string) any {
	var paths []string
	if declared != "" {
		paths = []string{declared}
	} else {
		paths = defaults
	}
	for _, raw := range paths {
		segs, err := parseRel(raw)
		if err != nil {
			continue
		}
		segs = stripBinding(segs, bindingSegs(c))
		if v := resolveSegs(rec, segs); v != nil {
			return v
		}
	}
	return nil
}

// fieldValue resolves a named output field: contract binding fields win over
// the default candidate paths.
func fieldValue(c *contracts.Contract, name string, rec map[string]any, defaults []string) any {
	var declared string
	if p, ok := c.Binding.Fields[name]; ok {
		declared = p
	}
	return resolveValue(c, declared, rec, defaults)
}

func fieldStr(c *contracts.Contract, name string, rec map[string]any, defaults []string) string {
	if v := fieldValue(c, name, rec, defaults); v != nil {
		return asStr(v)
	}
	return ""
}

func fieldInt(c *contracts.Contract, name string, rec map[string]any, defaults []string) int64 {
	if v := fieldValue(c, name, rec, defaults); v != nil {
		return asInt(v)
	}
	return 0
}

func fieldBool(c *contracts.Contract, name string, rec map[string]any, defaults []string) bool {
	if v := fieldValue(c, name, rec, defaults); v != nil {
		return asBool(v)
	}
	return false
}

// extraFields captures contract fields named "extra.<key>" into the output
// struct's Extra map (best-effort, values passed through as-is).
func extraFields(c *contracts.Contract, rec map[string]any) map[string]any {
	var out map[string]any
	for name, p := range c.Binding.Fields {
		if !strings.HasPrefix(name, "extra.") {
			continue
		}
		if v := resolveValue(c, p, rec, nil); v != nil {
			if out == nil {
				out = map[string]any{}
			}
			out[strings.TrimPrefix(name, "extra.")] = v
		}
	}
	return out
}

// defaultAuthorCandidates are the plausible keys holding the author/user
// object on a comment or item record.
var defaultAuthorCandidates = []string{
	"user", "user_info", "author", "authorInfo", "creator", "owner",
	"aweme_info.author", "note_card.user", "noteCard.user",
}

// authorObject returns the author record from an item/comment record: either
// the object declared under one of the author alias field names, or the first
// default candidate that is a map.
func authorObject(c *contracts.Contract, rec map[string]any, aliases []string) map[string]any {
	for _, a := range aliases {
		if p, ok := c.Binding.Fields[a]; ok {
			if m, ok := resolveValue(c, p, rec, nil).(map[string]any); ok && m != nil {
				return m
			}
		}
	}
	if len(aliases) == 0 {
		return nil
	}
	for _, raw := range defaultAuthorCandidates {
		if m, ok := resolveValue(c, "", rec, []string{raw}).(map[string]any); ok && m != nil {
			return m
		}
	}
	return nil
}

// userAliases are the field-name prefixes under which per-platform contracts
// declare author/user sub-fields ("user.uid", "author.nickname", ...).
var userAliases = []string{"user", "author", "user_info"}

// aliasField resolves a user sub-field: a declared path under any alias
// prefix (or the plain name) wins; otherwise nil so the caller falls back to
// defaults resolved against the author object.
func aliasField(c *contracts.Contract, f string, rec map[string]any, aliases []string) any {
	for _, a := range aliases {
		if p, ok := c.Binding.Fields[a+"."+f]; ok {
			if v := resolveValue(c, p, rec, nil); v != nil {
				return v
			}
		}
	}
	if p, ok := c.Binding.Fields[f]; ok {
		if v := resolveValue(c, p, rec, nil); v != nil {
			return v
		}
	}
	return nil
}

// bindUserFrom fills a UserProfile from rec. aliases != nil switches the
// default-path source to the author object (comment/item author binding);
// declared "user.<f>"/"author.<f>"/"user_info.<f>" field paths always resolve
// against the record itself.
func bindUserFrom(c *contracts.Contract, rec map[string]any, aliases []string, u *model.UserProfile) {
	obj := rec
	objSwitched := false
	if m := authorObject(c, rec, aliases); m != nil {
		obj = m
		objSwitched = true
	}
	set := func(f string, defaults []string, conv func(any) any, apply func(any)) {
		v := aliasField(c, f, rec, aliases)
		if v == nil && objSwitched {
			v = resolveValue(c, "", obj, defaults)
		} else if v == nil {
			v = resolveValue(c, "", rec, defaults)
		}
		if v != nil {
			apply(conv(v))
		}
	}
	set("uid", []string{"uid", "user_id", "id"}, asAnyStr, func(v any) { u.UID = v.(string) })
	set("sec_uid", []string{"sec_uid"}, asAnyStr, func(v any) { u.SecUID = v.(string) })
	set("short_id", []string{"short_id", "shortId"}, asAnyStr, func(v any) { u.ShortID = v.(string) })
	set("nickname", []string{"nickname", "name", "user_name", "display_name"}, asAnyStr, func(v any) { u.Nickname = v.(string) })
	set("avatar_url", []string{"avatar_url", "avatar", "avatarUrl", "head_url", "headUrl", "photo_url"}, asAnyStr, func(v any) { u.AvatarURL = v.(string) })
	set("signature", []string{"signature", "profile_bio"}, asAnyStr, func(v any) { u.Signature = v.(string) })
	set("ip_label", []string{"ip_label", "ip_location", "ipLocation"}, asAnyStr, func(v any) { u.IPLabel = v.(string) })
	set("gender", []string{"gender"}, asAnyGender, func(v any) { u.Gender = v.(int) })
	set("follower_count", []string{"follower_count", "fans_count", "fans"}, asAnyInt, func(v any) { u.FollowerCount = v.(int64) })
	set("following_count", []string{"following_count", "follow_count", "follows"}, asAnyInt, func(v any) { u.FollowingCount = v.(int64) })
	set("aweme_count", []string{"aweme_count", "notes_count", "note_count"}, asAnyInt, func(v any) { u.AwemeCount = v.(int64) })
	set("total_favorited", []string{"total_favorited", "favorited_count"}, asAnyInt, func(v any) { u.TotalFavorited = v.(int64) })
	u.Extra = extraFields(c, rec)
}

// asAnyStr / asAnyInt adapt value converters to the set() plumbing above.
func asAnyStr(v any) any { return asStr(v) }

func asAnyInt(v any) any { return asInt(v) }

// asAnyGender adapts genderFrom to the set() plumbing above.
func asAnyGender(v any) any { return genderFrom(v) }

// genderFrom normalizes platform gender values into the model convention
// (0 unknown, 1 male, 2 female). Numbers pass through; kuaishou faces render
// gender as "M"/"F" strings (corpus /rest/v/profile/get "sex":"M"), and the
// common word/CJK forms map too. Unmappable values stay 0 (unknown).
func genderFrom(v any) int {
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "m", "male", "男":
			return 1
		case "f", "female", "女":
			return 2
		}
	}
	return int(asInt(v))
}

// bindUser binds a top-level user/profile record (users binding).
func bindUser(c *contracts.Contract, rec map[string]any) model.UserProfile {
	var u model.UserProfile
	bindUserFrom(c, rec, nil, &u)
	return u
}

// bindItem maps one raw search-result record into a model.Item.
func bindItem(c *contracts.Contract, rec map[string]any) model.Item {
	var it model.Item
	it.ID = fieldStr(c, "id", rec, []string{
		"id", "aweme_id", "aweme_info.aweme_id", "photo.id", "note_id", "note_card.id", "noteCard.id", "collects_id",
	})
	it.Desc = fieldStr(c, "desc", rec, []string{
		"desc", "aweme_info.desc", "caption", "aweme_info.caption", "title", "photo.caption",
		"note_card.display_title", "noteCard.displayTitle",
	})
	it.CoverURL = fieldStr(c, "cover_url", rec, []string{
		"cover_url", "cover", "aweme_info.cover_url", "aweme_info.cover",
		"video.cover.url_list[0]", "aweme_info.video.cover.url_list[0]", "note_card.cover",
	})
	it.CreateTime = fieldInt(c, "create_time", rec, []string{
		"create_time", "aweme_info.create_time", "timestamp", "photo.timestamp", "note_card.create_time",
	})
	bindUserFrom(c, rec, []string{"author", "user"}, &it.Author)
	it.MediaType = mediaType(c, rec)
	it.Stats.Digg = fieldInt(c, "stats.digg", rec, []string{
		"statistics.digg_count", "stats.digg_count", "digg_count", "like_count", "photo.like_count",
	})
	it.Stats.Comment = fieldInt(c, "stats.comment", rec, []string{
		"statistics.comment_count", "stats.comment_count", "comment_count",
	})
	it.Stats.Collect = fieldInt(c, "stats.collect", rec, []string{
		"statistics.collect_count", "stats.collect_count", "collect_count",
	})
	it.Stats.Share = fieldInt(c, "stats.share", rec, []string{
		"statistics.share_count", "stats.share_count", "share_count",
	})
	it.Extra = extraFields(c, rec)
	return it
}

// mediaType classifies an item: string declarations, numeric conventions
// (douyin: 1 video, 68 image) and structural evidence (non-empty image list).
func mediaType(c *contracts.Contract, rec map[string]any) string {
	if v := fieldValue(c, "media_type", rec, []string{
		"media_type", "type", "aweme_info.type", "note_card.type", "noteCard.type", "model_type",
	}); v != nil {
		switch t := v.(type) {
		case string:
			switch strings.ToLower(t) {
			case "video":
				return "video"
			case "image", "normal", "photo", "note", "picture":
				return "image"
			}
		case float64:
			switch int64(t) {
			case 1:
				return "video"
			case 2, 68:
				return "image"
			}
		}
	}
	for _, raw := range []string{"image_infos", "aweme_info.image_infos", "images", "image_list", "aweme_info.images"} {
		if arr, ok := resolveValue(c, "", rec, []string{raw}).([]any); ok && len(arr) > 0 {
			return "image"
		}
	}
	return "unknown"
}

// bindComment maps one raw comment record into a model.Comment.
func bindComment(c *contracts.Contract, rec map[string]any) model.Comment {
	var cm model.Comment
	cm.CID = fieldStr(c, "cid", rec, []string{"cid", "comment_id", "id", "commentId"})
	cm.AwemeID = fieldStr(c, "aweme_id", rec, []string{"aweme_id", "item_id", "photo_id", "note_id"})
	cm.Text = fieldStr(c, "text", rec, []string{"text", "content"})
	cm.CreateTime = fieldInt(c, "create_time", rec, []string{"create_time", "ctime"})
	cm.DiggCount = fieldInt(c, "digg_count", rec, []string{"digg_count", "liked_count", "like_count"})
	cm.ReplyCount = fieldInt(c, "reply_count", rec, []string{"reply_count"})
	cm.Sticky = fieldBool(c, "sticky", rec, []string{"sticky", "is_top", "isSticky"})
	cm.ReplyToCID = fieldStr(c, "reply_to_cid", rec, []string{"reply_to_cid", "reply_id"})
	bindUserFrom(c, rec, userAliases, &cm.User)
	cm.Extra = extraFields(c, rec)
	return cm
}

// bindMember maps one raw group-member record into a model.GroupMember.
func bindMember(c *contracts.Contract, rec map[string]any, groupID string) model.GroupMember {
	var gm model.GroupMember
	var u model.UserProfile
	bindUserFrom(c, rec, nil, &u)
	gm.UserProfile = u
	gm.GroupID = groupID
	gm.JoinedAt = fieldInt(c, "joined_at", rec, []string{"joined_at", "join_time"})
	return gm
}

// asStr renders v for string destinations.
func asStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return asStr(float64(t))
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case uint:
		return strconv.FormatUint(uint64(t), 10)
	case jsonNumber:
		return t.String()
	}
	return fmt.Sprintf("%v", v)
}

// asInt renders v for int64 destinations (best-effort parse).
func asInt(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return int64(t)
	case float32:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case int32:
		return int64(t)
	case uint64:
		return int64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	case jsonNumber:
		n, _ := t.Int64()
		return n
	}
	return 0
}

// asBool renders v for boolean destinations (numbers 0/1, "true"/"false").
func asBool(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case float32:
		return t != 0
	case int64:
		return t != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1":
			return true
		case "false", "0", "":
			return false
		}
		return asInt(v) != 0
	case jsonNumber:
		n, _ := t.Int64()
		return n != 0
	}
	return false
}

// jsonNumber is the encoding/json number type when UseNumber is active.
type jsonNumber interface {
	String() string
	Int64() (int64, error)
	Float64() (float64, error)
}
