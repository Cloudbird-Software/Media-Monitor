// topic.go — TopicFeed, the topic/hashtag dimension atom (capability H,
// P3): given a topic or hashtag, collect the content list under it through
// the platform's own topic-entry face, plus whatever topic metadata the
// reachable face carries.
//
// Design notes (atom boundary / schema trade-offs):
//   - Zero new endpoints: every platform's REACHABLE topic entry is its
//     search face (the proposals exclude no face here — the dedicated
//     hashtag/challenge endpoints are simply not implemented on the synth
//     oracle, and "the collectable face is the standard"): dy searches the
//     hashtag word and anchors attribution on text_extra (话题锚文本, the
//     hashtag_name/hashtag_id pairs each card carries), ks rides the search
//     topic stream with photo-level tags, xhs has NO tag face on the oracle
//     (note_card carries no tag structure — documented absent, the content
//     list still collects through search).
//   - The topic-attribution key families are contract data first: binding
//     fields "topic.tags" / "topic.tag_id" override the shape defaults
//     (text_extra[*].hashtag_name family, tags[*].name family) — same
//     contract-fields-override discipline as the suggest word families.
//   - Anchoring is exact-match after '#' normalization (a hashtag feed item
//     may carry several tags; the anchored count only counts items that
//     anchor THIS topic). HashtagID is the first id seen at the topic's
//     index — the parallel-array position of the matching anchor.
//   - The walk reuses the search pagination machinery wholesale:
//     fetchPagesDedup with the item id as key (cursor rewind guard), so the
//     count clamp, pacing, dedup and page guard are all inherited.
package collect

import (
	"context"
	"fmt"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
)

// TopicMeta is the topic-level metadata the reachable face carries ("if
// present": dy hashtag ids exist; ks tags are name-only; xhs has no face).
type TopicMeta struct {
	// HashtagID is the topic's platform id at its anchor (dy text_extra
	// hashtag_id — the challenge id family; empty when the face carries none).
	HashtagID string `json:"hashtag_id,omitempty"`
	// AnchorFace names where the anchors came from: text_extra | tags |
	// contract (declared field) | "" (no tag face on this platform).
	AnchorFace string `json:"anchor_face,omitempty"`
}

// TopicResult is the collected topic dimension for one topic word.
type TopicResult struct {
	Site  string       `json:"site"`
	Topic string       `json:"topic"`
	Items []model.Item `json:"items"`
	// AnchoredItems counts items whose anchors carry this exact topic.
	AnchoredItems int `json:"anchored_items"`
	// Walked counts records seen on the wire (id-less cards excluded from
	// Items are still counted here).
	Walked int       `json:"walked"`
	Pages  int       `json:"pages"`
	Dupes  int       `json:"dupes"`
	Meta   TopicMeta `json:"meta"`
}

// TopicOptions tunes the topic walk.
type TopicOptions struct {
	// Limit caps the collected item count (0 = walk to the face's natural
	// termination: has_more end or the dedup/page guards).
	Limit int
}

// topicTagDefaults are the shape-default anchor families per structural face
// (contract binding fields "topic.tags"/"topic.tag_id" override these): the
// dy hashtag anchors live in text_extra, the ks feed tags at record level.
var topicTagDefaults = []struct {
	face  string
	paths []string
}{
	{"text_extra", []string{"text_extra[*].hashtag_name", "aweme_info.text_extra[*].hashtag_name"}},
	{"tags", []string{"tags[*].name"}},
}

var topicTagIDDefaults = []string{
	"text_extra[*].hashtag_id",
	"aweme_info.text_extra[*].hashtag_id",
}

// topicAnchors resolves one record's anchor tags and their parallel ids:
// the contract-declared paths win (face "contract"); otherwise the shape
// families are tried in order and the first yielding tags sets the face.
func topicAnchors(c *contracts.Contract, rec map[string]any) (tags, ids []string, face string) {
	if p := c.Binding.Fields["topic.tags"]; p != "" {
		tags = resolveAll(c, p, rec, nil)
		if len(tags) > 0 {
			face = "contract"
		}
	} else {
		for _, fam := range topicTagDefaults {
			tags = resolveAll(c, "", rec, fam.paths)
			if len(tags) > 0 {
				face = fam.face
				break
			}
		}
	}
	if face == "" {
		return nil, nil, ""
	}
	if p := c.Binding.Fields["topic.tag_id"]; p != "" {
		ids = resolveAll(c, p, rec, nil)
	} else if face == "text_extra" || face == "contract" {
		ids = resolveAll(c, "", rec, topicTagIDDefaults)
	}
	return tags, ids, face
}

// TopicFeed collects the content list under one topic/hashtag through the
// platform's search face, anchoring each item's topic attribution from the
// platform's tag structure. An empty/whitespace topic fails closed.
func (e *Engine) TopicFeed(ctx context.Context, platform, topic string, opt TopicOptions) (TopicResult, error) {
	var out TopicResult
	topic = strings.TrimSpace(topic)
	topic = strings.TrimPrefix(topic, "#")
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return out, fmt.Errorf("collect: topic feed: empty topic")
	}
	name, err := e.resolveName(platform, "search")
	if err != nil {
		return out, err
	}
	c, ok := e.reg.Get(name)
	if !ok {
		return out, fmt.Errorf("collect: contract %q not registered", name)
	}
	walk, werr := e.fetchPagesDedup(ctx, name,
		map[string]string{"keyword": topic}, nil, model.Cursor{}, opt.Limit,
		func(rec map[string]any) string {
			return fieldStr(c, "id", rec, []string{
				"aweme_id", "aweme_info.aweme_id", "photo.id", "note_id", "id",
			})
		})
	if werr != nil && len(walk.records) == 0 {
		return out, werr
	}
	out.Site = platform
	out.Topic = topic
	out.Walked = walk.fetched
	out.Pages = walk.pages
	out.Dupes = walk.dupes
	for _, rec := range walk.records {
		it := bindItem(c, rec)
		if it.ID == "" {
			continue // id-less cards (e.g. xhs hot_query) are not content
		}
		out.Items = append(out.Items, it)
		tags, ids, face := topicAnchors(c, rec)
		if face != "" && out.Meta.AnchorFace == "" {
			out.Meta.AnchorFace = face
		}
		for i, tag := range tags {
			if tag != topic {
				continue
			}
			out.AnchoredItems++
			if out.Meta.HashtagID == "" && i < len(ids) {
				out.Meta.HashtagID = ids[i]
			}
			break
		}
	}
	e.obsInc("collect.topic_feed", 1)
	if out.AnchoredItems > 0 {
		e.obsInc("collect.topic_anchored", int64(out.AnchoredItems))
	}
	return out, werr
}
