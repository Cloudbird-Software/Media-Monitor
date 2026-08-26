package collect

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/testkit"
)

var propComment = &contracts.Contract{Name: "prop-comments", Platform: "mock", Category: "comments",
	Binding: contracts.Binding{Comments: "$.comments"}}

var propCommentFields = &contracts.Contract{
	Name: "prop-comments-f", Platform: "mock", Category: "comments",
	Binding: contracts.Binding{
		Comments: "$.comments",
		Fields: map[string]string{
			"text":       "$.comments[].text",
			"user.uid":   "$.comments[].user.uid",
			"digg_count": "$.comments[].digg",
		},
	},
}

// TestPropBindCommentKnownValues: for any random record, an injected known
// comment id/text round-trips through the default binding.
func TestPropBindCommentKnownValues(t *testing.T) {
	p := testkit.Prop{
		Name: "known_comment_fields_survive",
		Inv: func(r *testkit.R) string {
			m := r.Map(3, 4, 6)
			m["cid"] = "cid-" + strconv.FormatInt(r.Int63n(1<<30), 10)
			m["text"] = "text-fixed"
			m["user"] = map[string]any{"nickname": "nick-fixed", "uid": "u-fixed"}
			cm := bindComment(propComment, m)
			if cm.CID != m["cid"] {
				return fmt.Sprintf("CID = %q, want %q", cm.CID, m["cid"])
			}
			if cm.Text != "text-fixed" {
				return fmt.Sprintf("Text = %q", cm.Text)
			}
			if cm.User.Nickname != "nick-fixed" || cm.User.UID != "u-fixed" {
				return fmt.Sprintf("author = %+v", cm.User)
			}
			return ""
		},
	}
	testkit.Run(t, 20260201, 50, []testkit.Prop{p})
}

// TestPropBindFieldPaths: declared "root prefix + element relative" field
// paths resolve against noisy random documents.
func TestPropBindFieldPaths(t *testing.T) {
	p := testkit.Prop{
		Name: "declared_field_paths_resolve",
		Inv: func(r *testkit.R) string {
			rec := r.Map(2, 3, 5)
			rec["text"] = "declared-text"
			rec["user"] = map[string]any{"uid": "declared-uid"}
			rec["digg"] = float64(7)
			cm := bindComment(propCommentFields, rec)
			if cm.Text != "declared-text" {
				return fmt.Sprintf("Text = %q, want declared-text", cm.Text)
			}
			if cm.User.UID != "declared-uid" {
				return fmt.Sprintf("User.UID = %q", cm.User.UID)
			}
			if cm.DiggCount != 7 {
				return fmt.Sprintf("DiggCount = %d, want 7", cm.DiggCount)
			}
			return ""
		},
	}
	testkit.Run(t, 20260202, 50, []testkit.Prop{p})
}

// TestPropNumericConversions: int64 values inside the exact float64 range
// round-trip through the asStr/asInt conversions.
func TestPropNumericConversions(t *testing.T) {
	p := testkit.Prop{
		Name: "numeric_conversions_roundtrip",
		Inv: func(r *testkit.R) string {
			v := r.Int63n(1 << 40)
			doc := map[string]any{"digg_count": v, "create_time": v}
			cm := bindComment(propComment, doc)
			if cm.DiggCount != v {
				return fmt.Sprintf("DiggCount = %d, want %d", cm.DiggCount, v)
			}
			if cm.CreateTime != v {
				return fmt.Sprintf("CreateTime = %d, want %d", cm.CreateTime, v)
			}
			if s := asStr(v); s != strconv.FormatInt(v, 10) {
				return fmt.Sprintf("asStr(%d) = %q", v, s)
			}
			if asInt(float64(v)) != v {
				return fmt.Sprintf("asInt(float64(%d)) = %d", v, asInt(float64(v)))
			}
			return ""
		},
	}
	testkit.Run(t, 20260203, 50, []testkit.Prop{p})
}

// TestPropBindMemberNeverPanics: random records of any shape never crash the
// member binder and always carry the injected group id.
func TestPropBindMemberNeverPanics(t *testing.T) {
	p := testkit.Prop{
		Name: "member_binder_total",
		Inv: func(r *testkit.R) string {
			m := r.Map(3, 4, 6)
			gm := bindMember(propComment, m, "g-42")
			if gm.GroupID != "g-42" {
				return fmt.Sprintf("GroupID = %q", gm.GroupID)
			}
			_ = bindUser(propComment, m)
			_ = bindItem(propComment, m)
			_ = bindComment(propCommentFields, m)
			return ""
		},
	}
	testkit.Run(t, 20260204, 50, []testkit.Prop{p})
}

// TestPropUserDefaultsConsistency: a user object bound twice yields identical
// known fields (determinism of default-path resolution).
func TestPropUserDefaultsConsistency(t *testing.T) {
	p := testkit.Prop{
		Name: "user_binding_deterministic",
		Inv: func(r *testkit.R) string {
			m := r.Map(2, 3, 5)
			m["uid"] = strconv.FormatInt(r.Int63n(1<<32), 10)
			m["nickname"] = "n" + strconv.FormatInt(r.Int63n(1<<20), 10)
			m["gender"] = r.Int63n(3)
			u1 := bindUser(propComment, m)
			u2 := model.UserProfile{}
			bindUserFrom(propComment, m, nil, &u2)
			if u1.UID != u2.UID || u1.Nickname != u2.Nickname || u1.Gender != u2.Gender {
				return fmt.Sprintf("u1=%+v u2=%+v", u1, u2)
			}
			return ""
		},
	}
	testkit.Run(t, 20260205, 50, []testkit.Prop{p})
}
