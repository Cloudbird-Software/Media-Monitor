package live

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/wsutil"
)

// ErrRoomEnd is the sentinel a handler returns to stop monitoring: Connect
// treats it as a clean end and returns nil.
var ErrRoomEnd = errors.New("live: room ended by handler")

// errHandlerFatal wraps handler failures; Connect returns them without
// reconnecting — a broken consumer cannot be repaired by redialing.
var errHandlerFatal = errors.New("live: handler failed")

const (
	metaContractName       = "douyin-meta"
	roomIDField            = "room_id"
	defaultReconnectMax    = 3
	defaultHeartbeat       = 10 * time.Second
	reconnectBackoff       = 200 * time.Millisecond
	controlStatusStreamEnd = 3
)

// Config wires one live-monitor session. HTTP and Registry are required;
// Signer must be set (fail-closed — no silent unsigned dialing).
type Config struct {
	HTTP              *httpclient.Client // room page fetches (UA/retry)
	Registry          *contracts.Registry
	methods           map[string]string // protoName -> event key (contract protocol_methods)
	Signer            SignFn
	Obs               *obs.CounterMap
	ReconnectMax      int           // <=0 defaults to 3
	HeartbeatInterval time.Duration // <=0 defaults to 10s
}

// SignFn computes the wss "signature" query parameter. urlQuery is the
// assembled query string (everything after "?", without signature) and
// params is the same content as a map; the set of params the signature
// covers is entirely the SignFn's decision (X-MS-STUB style: md5 over the
// sorted param string).
type SignFn func(urlQuery string, params map[string]string) (signature string, err error)

// protocolMethods loads the contract's method table (proto name -> event
// key) with the built-in defaults when absent (declared fallback).
func (c *Config) protocolMethods() (map[string]string, error) {
	if meta, ok := c.Registry.Get(metaContractName); ok && meta != nil && len(meta.ProtoMethods) > 0 {
		out := map[string]string{}
		for key, proto := range meta.ProtoMethods {
			out[proto] = key
		}
		return out, nil
	}
	return defaultProtocolMethods(), nil
}

// Connect monitors one douyin live room until the stream ends (control
// status==3), the handler returns ErrRoomEnd, the context is canceled, or
// connection failures exceed ReconnectMax. Events are delivered to handler
// in wire order.
func (c *Config) Connect(ctx context.Context, roomURL string, handler func(ev model.LiveEvent) error) error {
	if c == nil {
		return errors.New("live: nil Connector")
	}
	if c.HTTP == nil {
		return errors.New("live: HTTP client required (Config.HTTP)")
	}
	if c.Registry == nil {
		return errors.New("live: contract registry required (Config.Registry)")
	}
	if c.Signer == nil {
		// Fail closed: never dial without an injected signature.
		return errors.New("live: no websocket signature signer configured (Config.Signer); refusing to connect unsigned")
	}
	if handler == nil {
		return errors.New("live: event handler required")
	}
	maxReconnect := c.ReconnectMax
	if maxReconnect <= 0 {
		maxReconnect = defaultReconnectMax
	}
	heartbeat := c.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeat
	}

	pageURL, err := normalizeRoomURL(c.Registry, roomURL)
	if err != nil {
		return err
	}
	methods, err := c.protocolMethods()
	if err != nil {
		return err
	}
	if err != nil {
		return err
	}
	roomID, err := fetchRoomID(ctx, c.HTTP, c.Registry, pageURL)
	if err != nil {
		return err
	}
	_ = methods

	var cursor string // last server cursor, carried into reconnects
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		serr := c.runSession(ctx, roomID, &cursor, heartbeat, handler)
		switch {
		case serr == nil: // control status==3 or ErrRoomEnd
			return nil
		case errors.Is(serr, context.Canceled), errors.Is(serr, context.DeadlineExceeded):
			return serr
		case errors.Is(serr, errHandlerFatal):
			return serr
		}
		if failures >= maxReconnect {
			return fmt.Errorf("live: session ended after %d reconnect attempt(s) (limit %d); last error: %w",
				failures, maxReconnect, serr)
		}
		failures++
		c.inc("live.reconnect", 1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnectBackoff):
		}
	}
}

// runSession runs one websocket session: signed dial, heartbeat, ack and the
// read loop, until a clean end, an error, or ctx cancellation.
func (c *Config) runSession(ctx context.Context, roomID string, cursor *string, heartbeat time.Duration, handler func(ev model.LiveEvent) error) error {
	wssURL, err := c.buildWSSURL(roomID, *cursor)
	if err != nil {
		return err
	}
	conn, err := wsutil.Dial(ctx, wssURL, wssHeaders(c.HTTP, c.Registry))
	if err != nil {
		return fmt.Errorf("live: dial: %w", err)
	}
	defer conn.Close()
	c.inc("live.connect", 1)

	hbCtx, hbStop := context.WithCancel(ctx)
	defer hbStop()
	go hbLoop(hbCtx, conn, c, heartbeat)

	for {
		raw, err := conn.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("live: read: %w", err)
		}
		body, err := maybeGunzip(raw)
		if err != nil {
			return fmt.Errorf("live: decompress: %w", err)
		}
		resp, err := DecodeResponse(body)
		if err != nil {
			return fmt.Errorf("live: response decode: %w", err)
		}
		if resp.Cursor != "" {
			*cursor = resp.Cursor
		}

		// Ack every non-control response once (control responses terminate
		// the session and are never acked); decode first to know the family.
		type decodedMsg struct {
			method  string
			payload []byte
			ok      bool
		}
		decoded := make([]decodedMsg, 0, len(resp.Messages))
		hasNonControl := false
		for _, msg := range resp.Messages {
			method, payload, ok := DecodeMessage(msg)
			decoded = append(decoded, decodedMsg{method, payload, ok})
			if ok && method != methodControl {
				hasNonControl = true
			}
		}
		if hasNonControl {
			c.sendAck(conn, resp)
		}

		for _, dm := range decoded {
			if !dm.ok {
				c.inc("live.error", 1)
				continue
			}
			fallbackNow := time.Now().Unix()
			if resp.Now > 0 {
				fallbackNow = int64(resp.Now)
			}
			ev, status, recognized := EventFromMessage(roomID, dm.method, dm.payload, fallbackNow)
			if !recognized {
				continue
			}
			if dm.method == methodControl {
				// Non-terminal control messages are housekeeping and are not
				// delivered; the terminal one (status==3) is delivered as the
				// final event and then ends the session.
				if status != controlStatusStreamEnd {
					continue
				}
				c.inc("live.control_end", 1)
				c.inc("live.event", 1)
				if err := handler(ev); err != nil {
					if errors.Is(err, ErrRoomEnd) {
						return nil
					}
					return fmt.Errorf("%w: %v", errHandlerFatal, err)
				}
				return nil
			}
			c.inc("live.event", 1)
			if err := handler(ev); err != nil {
				if errors.Is(err, ErrRoomEnd) {
					return nil
				}
				return fmt.Errorf("%w: %v", errHandlerFatal, err)
			}
		}
	}
}

// hbLoop sends a heartbeat PushFrame every interval until ctx is done or a
// write fails (a dead socket also surfaces in the read loop).
func hbLoop(ctx context.Context, conn *wsutil.Conn, c *Config, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	frame := EncodePushFrame(PushFrame{PayloadType: "hb"})
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.WriteBinary(frame); err != nil {
				return
			}
			c.inc("live.hb", 1)
		}
	}
}

// sendAck acknowledges a received response: PushFrame{payload_type:"ack",
// payload: internal_ext}. The simplified downlink envelope carries no frame
// log_id, so the ack logid stays 0; the server's internal_ext is echoed as
// the ack token.
func (c *Config) sendAck(conn *wsutil.Conn, resp Response) {
	var payload []byte
	if resp.InternalExt != "" {
		payload = []byte(resp.InternalExt)
	}
	if err := conn.WriteBinary(EncodePushFrame(PushFrame{PayloadType: "ack", Payload: payload})); err != nil {
		return
	}
	c.inc("live.ack", 1)
}

// maybeGunzip decompresses frames whose first two bytes are the gzip magic
// (the channel negotiates compress=gzip); uncompressed frames pass through.
func maybeGunzip(b []byte) ([]byte, error) {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return b, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// wssEndpointRoot is the im-push host. MEDIAMON_LIVE_WSS_ENDPOINT
// (test-only escape hatch, never set in production) redirects the endpoint
// to a local server; wsutil accepts http:// as a ws:// alias.
func wssEndpointRoot() string {
	if e := os.Getenv("MEDIAMON_LIVE_WSS_ENDPOINT"); e != "" {
		return strings.TrimRight(e, "/")
	}
	return "wss://webcast100-ws-web-lq.douyin.com"
}

// buildWSSURL assembles the signed websocket URL: the fixed im-push query
// template plus cursor, then "&signature=" with the SignFn result appended
// last (the signature covers the params the SignFn chooses from urlQuery).
// buildWSSURL assembles the wss dial URL entirely from the douyin-meta
// contract's transport_ws section (host/path/fixed params/runtime-param
// names). Env override MEDIAMON_LIVE_WSS_ENDPOINT replaces scheme://host,
// matching the page endpoint pattern. Fail-closed when the section (or any
// required part) is missing.
func (c *Config) buildWSSURL(roomID, cursor string) (string, error) {
	meta, ok := c.Registry.Get(metaContractName)
	if !ok {
		return "", fmt.Errorf("live: contract %q not registered", metaContractName)
	}
	tws := meta.TransportWS
	if tws == nil || tws.Path == "" || tws.WSSHost == "" {
		return "", fmt.Errorf("live: contract %q declares no transport_ws (wss_host+path)", metaContractName)
	}
	root := "wss://" + tws.WSSHost
	if e := os.Getenv("MEDIAMON_LIVE_WSS_ENDPOINT"); e != "" {
		root = strings.TrimRight(e, "/")
	}
	q := url.Values{}
	for k, v := range tws.Params {
		q.Set(k, v)
	}
	for _, rp := range tws.RuntimeParams {
		switch rp {
		case "user_unique_id":
			q.Set("user_unique_id", randomUniqueID())
		case "room_id":
			q.Set("room_id", roomID)
		case "cursor":
			q.Set("cursor", cursor)
		default:
			return "", fmt.Errorf("live: contract %q declares unknown runtime param %q", metaContractName, rp)
		}
	}
	base := root + tws.Path + "?" + q.Encode()
	params, err := queryParams(base)
	if err != nil {
		return "", err
	}
	query := base[strings.IndexByte(base, '?')+1:]
	sig, err := c.Signer(query, params)
	if err != nil {
		return "", fmt.Errorf("live: ws signature: %w", err)
	}
	return base + "&signature=" + url.QueryEscape(sig), nil
}

// queryParams decodes the query part of a built URL into a map (last value
// wins, matching how query dupes are read).
func queryParams(rawURL string) (map[string]string, error) {
	i := strings.IndexByte(rawURL, '?')
	if i < 0 {
		return nil, errors.New("live: ws url has no query")
	}
	vals, err := url.ParseQuery(rawURL[i+1:])
	if err != nil {
		return nil, fmt.Errorf("live: ws query parse: %w", err)
	}
	out := make(map[string]string, len(vals))
	for k, vs := range vals {
		if len(vs) > 0 {
			out[k] = vs[len(vs)-1]
		}
	}
	return out, nil
}

// randomUniqueID returns 19 random digits (the anonymous user_unique_id).
func randomUniqueID() string {
	var b [19]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable-ish; fall back to time digits.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte('0' + n%10)
			n /= 10
		}
		return string(b[:])
	}
	for i := range b {
		b[i] = '0' + b[i]%10
	}
	return string(b[:])
}

// wssHeaders builds the websocket handshake headers: a rotating UA from the
// HTTP client and an Origin derived from the douyin-meta contract base_url
// (no hardcoded URL here).
func wssHeaders(hc *httpclient.Client, reg *contracts.Registry) http.Header {
	h := http.Header{}
	if hc != nil {
		h.Set("User-Agent", hc.UA())
	}
	if origin := pageOrigin(reg); origin != "" {
		h.Set("Origin", origin)
	}
	return h
}

// pageOrigin derives the wss Origin header from the douyin-meta contract
// base_url (scheme://host).
func pageOrigin(reg *contracts.Registry) string {
	c, ok := reg.Get(metaContractName)
	if !ok {
		return ""
	}
	u, err := url.Parse(c.Transport.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// normalizeRoomURL validates an input room URL and normalizes it to the
// canonical live.douyin.com/{room_web} form declared by the douyin-meta
// contract (transport base_url + path placeholder; nothing else is
// hardcoded here).
func normalizeRoomURL(reg *contracts.Registry, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("live: room url is required")
	}
	c, ok := reg.Get(metaContractName)
	if !ok {
		return "", fmt.Errorf("live: contract %q not registered", metaContractName)
	}
	if len(c.Transport.Placeholders) == 0 {
		return "", fmt.Errorf("live: contract %q declares no path placeholders", c.Name)
	}
	u := raw
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	pu, err := url.Parse(u)
	if err != nil {
		return "", fmt.Errorf("live: parse room url: %w", err)
	}
	switch strings.ToLower(pu.Hostname()) {
	case "live.douyin.com", "www.live.douyin.com":
	default:
		return "", fmt.Errorf("live: unsupported room url host %q (want live.douyin.com)", pu.Hostname())
	}
	segs := strings.Split(strings.Trim(pu.Path, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		return "", errors.New("live: room url has no room_web path segment")
	}
	roomWeb := segs[0]
	if len(roomWeb) > 64 {
		return "", fmt.Errorf("live: room_web segment too long (%d bytes)", len(roomWeb))
	}
	path := c.Transport.Path
	for _, p := range c.Transport.Placeholders {
		tok := "{" + p + "}"
		if !strings.Contains(path, tok) {
			return "", fmt.Errorf("live: contract %q path %q misses placeholder %q", c.Name, path, tok)
		}
		path = strings.ReplaceAll(path, tok, url.PathEscape(roomWeb))
	}
	if strings.ContainsAny(path, "{}") {
		return "", fmt.Errorf("live: contract %q path has unfilled placeholders", c.Name)
	}
	return c.Transport.BaseURL + path, nil
}

// fetchRoomID GETs the room page and extracts the room id via the runtime
// locator semantics declared by the douyin-meta binding field "room_id".
func fetchRoomID(ctx context.Context, hc *httpclient.Client, reg *contracts.Registry, pageURL string) (string, error) {
	c, ok := reg.Get(metaContractName)
	if !ok {
		return "", fmt.Errorf("live: contract %q not registered", metaContractName)
	}
	// The binding KEY is the semantic field name whose camelCase form is the
	// page locator; the VALUE ($.room_id) is the JSON-document path form and
	// is kept only for the drift/declaration surface.
	if _, ok := c.Binding.Fields[roomIDField]; !ok {
		return "", fmt.Errorf("live: contract %q declares no binding field %q", c.Name, roomIDField)
	}
	status, body, err := hc.WithContract(c.Name).Do(ctx, http.MethodGet, pageURLBy(pageURL), nil, nil)
	if err != nil {
		return "", fmt.Errorf("live: room page: %w", err)
	}
	if status < 200 || status >= 300 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return "", fmt.Errorf("live: room page: status %d: %s", status, snippet)
	}
	roomID, err := extractRoomID(body, roomIDField)
	if err != nil {
		return "", fmt.Errorf("live: room page: %w", err)
	}
	return roomID, nil
}

// pageURLBy applies the test-only MEDIAMON_LIVE_PAGE_ENDPOINT override
// (replaces scheme://host of the canonical page URL with a local server).
func pageURLBy(pageURL string) string {
	if e := os.Getenv("MEDIAMON_LIVE_PAGE_ENDPOINT"); e != "" {
		u, err := url.Parse(pageURL)
		if err == nil {
			p := u.EscapedPath()
			if u.RawQuery != "" {
				p += "?" + u.RawQuery
			}
			return strings.TrimRight(e, "/") + p
		}
	}
	return pageURL
}

// extractRoomID finds the live room id on the room page. The locator is
// derived from the contract-declared binding field name (douyin-meta
// "room_id"): the page embeds the JSON string
//
//	"roomId":"<digits>"
//
// i.e. the camelCase form of the declared field name acting as the runtime
// locator for the HTML-embedded JSON document. The declared JSONPath value
// ("$.room_id") is the JSON-document form and is not applied to the HTML
// page.
func extractRoomID(page []byte, fieldName string) (string, error) {
	key := camelCase(fieldName)
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"([0-9]{1,20})"`)
	m := re.FindSubmatch(page)
	if m == nil {
		return "", fmt.Errorf("room_id locator %q (page key %q) not found", fieldName, key)
	}
	return string(m[1]), nil
}

// camelCase converts a snake_case binding field name to its page key form:
// room_id → roomId, user_count → userCount.
func camelCase(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func (c *Config) inc(name string, d int64) {
	if c.Obs != nil {
		c.Obs.Inc(name, d)
	}
}
