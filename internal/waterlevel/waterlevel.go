// Package waterlevel implements the account-pool water-level alarm
// (IR-MM-0001 AC-10): when a platform's usable (healthy + degraded,
// active) account count drops below a threshold, a type:drift issue is
// opened via the repo App token; when the level recovers, the issue is
// closed with a recovery note. Idempotent: one open alarm per platform.
//
// The GitHub-facing side is an interface so the decision logic is fully
// testable offline; the App-token notifier (AGENT_APP_SECRET, stdlib-only
// RS256 JWT) is the production implementation.
package waterlevel

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/accounts"
)

// DefaultThreshold is the default per-platform usable-account floor
// (overridable via MEDIAMON_WATERLEVEL_MIN).
const DefaultThreshold = 2

// Alert describes one platform below its water level.
type Alert struct {
	Platform  string
	Usable    int
	Threshold int
	// IDs are pool ids only — masked by construction: no cookie or
	// credential fragment ever rides an alert (INV-6).
	IDs []string
}

// Notifier is the issue-plane surface: open / find-open / close.
type Notifier interface {
	// OpenIssue opens the drift issue and returns its number.
	OpenIssue(a Alert) (int, error)
	// OpenIssueNumber returns the open water-level issue for a platform
	// (0 = none).
	OpenIssueNumber(platform string) (int, error)
	// CloseIssue closes an issue with a comment.
	CloseIssue(num int, comment string) error
}

// Check computes the per-platform alerts for the pool.
func Check(pool *accounts.Pool, threshold int) []Alert {
	if pool == nil || threshold <= 0 {
		return nil
	}
	usable := map[string][]string{}
	for _, a := range pool.List() {
		if a.Status != accounts.StatusActive {
			continue
		}
		switch a.Health {
		case accounts.HealthHealthy, accounts.HealthDegraded, "":
			usable[a.Platform] = append(usable[a.Platform], a.ID)
		}
	}
	var out []Alert
	for platform, ids := range usable {
		if len(ids) < threshold {
			sort.Strings(ids)
			out = append(out, Alert{Platform: platform, Usable: len(ids), Threshold: threshold, IDs: ids})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Platform < out[j].Platform })
	return out
}

// Run executes one alarm cycle (idempotent): opens issues for new
// low-water platforms, closes the open issue for recovered platforms.
func Run(pool *accounts.Pool, n Notifier, threshold int) (opened, closed []int, err error) {
	alerts := Check(pool, threshold)
	alerted := map[string]Alert{}
	for _, a := range alerts {
		alerted[a.Platform] = a
	}
	// platforms seen in the pool at all (recovery detection)
	seen := map[string]bool{}
	for _, a := range pool.List() {
		seen[a.Platform] = true
	}
	for platform := range seen {
		num, nerr := n.OpenIssueNumber(platform)
		if nerr != nil {
			return opened, closed, nerr
		}
		a, low := alerted[platform]
		switch {
		case low && num == 0:
			issue, oerr := n.OpenIssue(a)
			if oerr != nil {
				return opened, closed, oerr
			}
			opened = append(opened, issue)
		case !low && num != 0:
			if cerr := n.CloseIssue(num, fmt.Sprintf("水位恢复：平台 %s 可用账号已回到阈值（%d）之上。", platform, threshold)); cerr != nil {
				return opened, closed, cerr
			}
			closed = append(closed, num)
		}
	}
	return opened, closed, nil
}

// IssueTitle is the drift-issue title for an alert.
func (a Alert) IssueTitle() string {
	return fmt.Sprintf("drift: 账号池水位告警——%s 可用账号 %d 低于阈值 %d", a.Platform, a.Usable, a.Threshold)
}

// IssueBody renders the five required elements: platform / usable count /
// threshold / masked account ids / replenishment guide (ENV-REQ-2).
func (a Alert) IssueBody() string {
	return fmt.Sprintf(`## 账号池水位告警

- 平台：**%s**
- 当前可用账号（healthy+degraded）：**%d**
- 阈值：**%d**
- 账号（仅 pool id，脱敏）： %s

补号指引：按 ENV-REQ-2 cookie 导出指南（Cloudbird-Software/Media-Monitor#12）导出并
`+"`mediactl accounts import --platform %s --file <cookie.txt>` 导入后，`mediactl accounts probe --id <id>` 验证。", a.Platform, a.Usable, a.Threshold, strings.Join(a.IDs, ", "), a.Platform)
}
