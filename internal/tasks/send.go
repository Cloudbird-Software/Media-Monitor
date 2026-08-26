// Package tasks orchestrates action-type work that the collect engine performs
// one shot at a time: direct-message broadcasts, (later) trace/群控 flows.
// Platform endpoints are reached only through the engine's contract surface;
// this package owns the policy (templates, timing, caps, retry) that sits on
// top of one-shot engine actions.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

// MessageTemplate is one message in a broadcast sequence (the original
// software sends a first message, then an optional second after a delay).
type MessageTemplate struct {
	Content string `json:"content"` // literal text; {nickname} is replaced at send time
}

// SendTaskConfig is the parameters of one broadcast job.
type SendTaskConfig struct {
	Platform       string            `json:"platform"`                      // douyin|...
	Targets        []string          `json:"targets"`                       // sec_uid list
	FirstMessage   MessageTemplate   `json:"first_message"`                 // required
	SecondMessage  *MessageTemplate  `json:"second_message,omitempty"`      // optional second message
	SecondDelayMs  int64             `json:"second_delay_ms"`               // delay before second message (default 15000)
	SendCap        int               `json:"send_cap"`                      // max sends per account (0 = unlimited)
	AccountID      string            `json:"account_id,omitempty"`          // act as this account (empty = platform default)
	SubstituteNick map[string]string `json:"substitute_nickname,omitempty"` // sec_uid -> nickname for {nickname}
}

// SendOutcome is the result for one target.
type SendOutcome struct {
	Target       string `json:"target"`
	FirstStatus  string `json:"first_status"`
	SecondStatus string `json:"second_status,omitempty"`
	Error        string `json:"error,omitempty"`
}

// SendReport aggregates one broadcast job.
type SendReport struct {
	Platform string        `json:"platform"`
	Results  []SendOutcome `json:"results"`
	Skipped  int           `json:"skipped"` // capped targets
}

// Sender drives broadcast jobs. The store persists per-account send counts
// across runs; the inter-message delay clock is injectable via SetSleep so
// tests never sleep for real.
type Sender struct {
	eng   *collect.Engine
	st    *store.Store
	sleep func(ctx context.Context, d time.Duration) error
}

// NewSender builds a Sender. eng is required; st may be nil.
func NewSender(eng *collect.Engine, st *store.Store) *Sender {
	return &Sender{eng: eng, st: st, sleep: defaultSleep}
}

// SetSleep overrides the inter-message delay clock (tests inject a fake that
// records delays without real sleeping).
func (s *Sender) SetSleep(sleep func(ctx context.Context, d time.Duration) error) {
	s.sleep = sleep
}

// defaultSleep waits d or until ctx is done.
func defaultSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Run executes one broadcast job. Per-account send caps are enforced against
// the persisted counter; targets beyond the cap are skipped. Each send is
// attempted once and retried once on failure before recording the error and
// moving on (the original software gives up after the retry).
func (s *Sender) Run(ctx context.Context, cfg SendTaskConfig) (*SendReport, error) {
	if cfg.Platform == "" {
		return nil, errors.New("tasks: platform is required")
	}
	if len(cfg.Targets) == 0 {
		return nil, errors.New("tasks: targets is required")
	}
	if cfg.FirstMessage.Content == "" {
		return nil, errors.New("tasks: first_message.content is required")
	}
	if cfg.SecondDelayMs <= 0 {
		cfg.SecondDelayMs = 15000
	}

	rep := &SendReport{Platform: cfg.Platform}
	for _, target := range cfg.Targets {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		out := SendOutcome{Target: target}

		// Cap check happens before every individual send (first and second are
		// counted separately): once the per-account cap is reached, the rest is
		// skipped fail-closed.
		if cfg.SendCap > 0 && s.sendCount(cfg.AccountID, cfg.Platform) >= cfg.SendCap {
			rep.Skipped++
			continue
		}

		first := substitute(cfg.FirstMessage.Content, target, cfg.SubstituteNick)
		status, err := s.sendWithRetry(ctx, cfg.Platform, target, first)
		out.FirstStatus = status
		if err != nil {
			out.Error = err.Error()
			rep.Results = append(rep.Results, out)
			continue
		}
		s.bumpCount(cfg.AccountID, cfg.Platform)

		if cfg.SecondMessage != nil && cfg.SecondMessage.Content != "" {
			if cfg.SendCap > 0 && s.sendCount(cfg.AccountID, cfg.Platform) >= cfg.SendCap {
				// First message went out but the cap blocks the second: report
				// the partial outcome, don't count it as a fully-skipped target.
				rep.Results = append(rep.Results, out)
				continue
			}
			if cfg.SecondDelayMs > 0 {
				if err := s.sleep(ctx, time.Duration(cfg.SecondDelayMs)*time.Millisecond); err != nil {
					return rep, err
				}
			}
			second := substitute(cfg.SecondMessage.Content, target, cfg.SubstituteNick)
			st, serr := s.sendWithRetry(ctx, cfg.Platform, target, second)
			out.SecondStatus = st
			if serr != nil {
				out.Error = serr.Error()
			} else {
				s.bumpCount(cfg.AccountID, cfg.Platform)
			}
		}

		rep.Results = append(rep.Results, out)
	}
	return rep, nil
}

// sendWithRetry tries once and, on failure, retries once. A non-empty status
// with a nil error means at least one attempt succeeded.
func (s *Sender) sendWithRetry(ctx context.Context, platform, target, text string) (string, error) {
	res, err := s.eng.SendMessage(ctx, platform, target, text)
	if err == nil {
		return res.Status, nil
	}
	res, retryErr := s.eng.SendMessage(ctx, platform, target, text)
	if retryErr != nil {
		return "", fmt.Errorf("%v (retry failed: %v)", err, retryErr)
	}
	return res.Status, nil
}

// counterKey is the per-(account,platform) send-cap key in the store.
func counterKey(accountID, platform string) string {
	return "sendcap:" + platform + ":" + accountID
}

func (s *Sender) sendCount(accountID, platform string) int {
	if s.st == nil {
		return 0
	}
	key := counterKey(accountID, platform)
	var n int
	_ = s.st.Scan("counters", func(raw []byte) error {
		var row struct {
			Key   string `json:"key"`
			Value int    `json:"value"`
		}
		if err := json.Unmarshal(raw, &row); err == nil && row.Key == key {
			n = row.Value
		}
		return nil
	})
	return n
}

func (s *Sender) bumpCount(accountID, platform string) {
	if s.st == nil {
		return
	}
	key := counterKey(accountID, platform)
	_ = s.st.Append("counters", map[string]any{"key": key, "value": s.sendCount(accountID, platform) + 1})
}

func substitute(text, target string, nick map[string]string) string {
	if nick != nil {
		if n, ok := nick[target]; ok && n != "" {
			return strings.ReplaceAll(text, "{nickname}", n)
		}
	}
	return text
}
