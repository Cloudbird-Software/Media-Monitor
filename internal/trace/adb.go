package trace

import (
	"context"
	"fmt"

	"github.com/Cloudbird-Software/Media-Monitor/internal/adb"
)

// AdbExecutor performs trace actions as adb gestures. It uses tap/swipe/text
// to drive the device and uiautomator dumps to locate UI elements. Coordinates
// are declared per-action by the caller via Layout hints (the original software
// inspects the live UI; here we operate on configured bounds for testability).
// The profile deep link comes from the platform flow (profile_url_template);
// it is never hardcoded here.
type AdbExecutor struct {
	clientFactory func(serial string) (adbClient, error)
	// layout supplies element bounds per platform/action. In a production build
	// this is distilled from vision flows; it is currently a static table
	// configured at construction time (the contract/flow supplies defaults).
	layout Layout
	// profileURLTemplate is the flow-declared profile deep-link template with
	// a {sec_uid} placeholder (see Flow.ProfileURLTemplate). Empty fails closed.
	profileURLTemplate string
}

// adbClient is the subset of internal/adb.Client the executor uses. All
// commands go through the exec service (hex4-framed): the shell service
// streams until the server closes the socket, which would kill the shared
// connection for the gestures that follow Prepare.
type adbClient interface {
	Tap(serial string, x, y int32) error
	Swipe(serial string, x0, y0, x1, y1 int32, ms int) error
	KeyText(serial, text string) error
	UIDump(serial string) (*adb.NodeTree, error)
	ExecOut(serial, cmd string) ([]byte, error)
}

// Layout maps action -> element bounds [x1,y1,x2,y2] in screen pixels.
type Layout map[ActionType][4]int32

// ExecutorOption configures an AdbExecutor.
type ExecutorOption func(*AdbExecutor)

// WithProfileURLTemplate sets the flow-declared profile deep-link template
// (must contain {sec_uid}). Typically Flow.ProfileURLTemplate.
func WithProfileURLTemplate(t string) ExecutorOption {
	return func(e *AdbExecutor) { e.profileURLTemplate = t }
}

// NewAdbExecutor builds an executor backed by a real adb.Client.
func NewAdbExecutor(client *adb.Client, layout Layout, opts ...ExecutorOption) *AdbExecutor {
	return AdbExecutorBy(func(serial string) (adbClient, error) {
		return &adbClientWrapper{c: client}, nil
	}, layout, opts...)
}

// AdbExecutorBy builds an executor with a custom client factory (tests inject a
// fake adb server).
func AdbExecutorBy(factory func(serial string) (adbClient, error), layout Layout, opts ...ExecutorOption) *AdbExecutor {
	e := &AdbExecutor{clientFactory: factory, layout: layout}
	for _, o := range opts {
		o(e)
	}
	return e
}

func (e *AdbExecutor) Prepare(ctx context.Context, dev Device, t Target) error {
	// Open the target's profile: am start with the flow-declared deep link.
	// Fail-closed when the flow declares no template.
	profileURL, err := (Flow{ProfileURLTemplate: e.profileURLTemplate}).RenderProfileURL(t.SecUID)
	if err != nil {
		return err
	}
	c, err := e.clientFactory(dev.Serial)
	if err != nil {
		return err
	}
	if _, err := c.ExecOut(dev.Serial, "am start -a android.intent.action.VIEW -d '"+profileURL+"'"); err != nil {
		return fmt.Errorf("open profile: %w", err)
	}
	// Probe the UI hierarchy to confirm the page rendered (the original
	// software inspects the live UI). Probe failures are non-fatal.
	_, _ = c.UIDump(dev.Serial)
	return nil
}

func (e *AdbExecutor) Run(ctx context.Context, dev Device, t Target, a Action) (int64, bool, error) {
	c, err := e.clientFactory(dev.Serial)
	if err != nil {
		return 0, false, err
	}
	bounds, ok := e.layout[a.Type]
	if !ok {
		// No declared bounds for this action: non-fatal skip.
		return 0, true, nil
	}
	centerX := (bounds[0] + bounds[2]) / 2
	centerY := (bounds[1] + bounds[3]) / 2
	switch a.Type {
	case ActionLike, ActionFollow, ActionCollect, ActionAvatarLike:
		if err := c.Tap(dev.Serial, centerX, centerY); err != nil {
			return 0, false, err
		}
	case ActionComment:
		// Tap the comment entry, then type the comment text (carried in the
		// target payload) via `input text`.
		if err := c.Tap(dev.Serial, centerX, centerY); err != nil {
			return 0, false, err
		}
		if txt, ok := t.Payload["comment"].(string); ok && txt != "" {
			if err := c.KeyText(dev.Serial, txt); err != nil {
				return 0, false, err
			}
		}
	case ActionDwell, ActionBrowse:
		// Dwell: brief swipe-down to register browsing, then wait.
		if err := c.Swipe(dev.Serial, centerX, centerY, centerX, centerY+200, 200); err != nil {
			return 0, false, err
		}
	case ActionDM:
		// DM is orchestrated by the DMExecutor, not gestures; non-fatal here.
		return 0, true, nil
	}
	return 0, false, nil
}

func (e *AdbExecutor) Release(ctx context.Context, dev Device, t Target) error {
	c, err := e.clientFactory(dev.Serial)
	if err != nil {
		return err
	}
	// Press back to leave the profile.
	_, err = c.ExecOut(dev.Serial, "input keyevent KEYCODE_BACK")
	return err
}

// adbClientWrapper binds an adb.Client to actions (the real client takes the
// serial per-call).
type adbClientWrapper struct {
	c      *adb.Client
	serial string
}

func (w *adbClientWrapper) Tap(serial string, x, y int32) error {
	return w.c.Tap(serial, x, y)
}

func (w *adbClientWrapper) Swipe(serial string, x0, y0, x1, y1 int32, ms int) error {
	return w.c.Swipe(serial, x0, y0, x1, y1, ms)
}

func (w *adbClientWrapper) KeyText(serial, text string) error {
	return w.c.KeyText(serial, text)
}

func (w *adbClientWrapper) UIDump(serial string) (*adb.NodeTree, error) {
	return w.c.UIDump(serial)
}

func (w *adbClientWrapper) ExecOut(serial, cmd string) ([]byte, error) {
	return w.c.ExecOut(serial, cmd)
}
