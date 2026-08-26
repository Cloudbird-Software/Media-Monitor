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
	// metaContractName is the live-meta contract name for the default
	// (douyin) platform; other platforms resolve to "<platform>-meta".
	metaContractName       = "douyin-meta"
	defaultLivePlatform    = "douyin"
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
	// Platform selects the live-meta contract ("<platform>-meta"); "" keeps
	// the historic douyin default.
	Platform string
	// Decoder is the optional platform decoder. When set, runSession uses it
	// instead of the built-in douyin protobuf path — this is how kuaishou's
	// gunzip+base64 JSON frames plug into the same engine (see 3.1). nil falls
	// back to douyin protobuf decoding.
	Decoder Decoder
}

// metaName resolves the live-meta contract name for the configured platform.
func (c *Config) metaName() string {
	if c.Platform == "" || c.Platform == defaultLivePlatform {
		return metaContractName
	}
	return c.Platform + "-meta"
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
	if meta, ok := c.Registry.Get(c.metaName()); ok && meta != nil && len(meta.ProtoMethods) > 0 {
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

	pageURL, err := normalizeRoomURL(c.Registry, c.metaName(), roomURL)
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
	roomID, err := fetchRoomID(ctx, c.HTTP, c.Registry, c.metaName(), pageURL)
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
	conn, err := wsutil.Dial(ctx, wssURL, wssHeaders(c.HTTP, c.Registry, c.metaName()))
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

		// Decode one frame into zero or more messages. With a platform
		// Decoder (kuaishou) the decoder owns gunzip+base64+JSON and returns
		// already-mapped methods; otherwise fall back to the douyin protobuf
		// path (gunzip -> DecodeResponse -> DecodeMessage).
		frameCursor, ackToken, decoded, err := c.decodeFrame(raw)
		if err != nil {
			return err
		}
		if frameCursor != "" {
			*cursor = frameCursor
		}

		hasNonControl := false
		for _, dm := range decoded {
			if dm.OK && dm.Method != methodControl {
				hasNonControl = true
			}
		}
		if hasNonControl && ackToken != "" {
			c.sendAckWith(conn, ackToken)
		}

		now := time.Now().Unix()
		for _, dm := range decoded {
			if !dm.OK {
				c.inc("live.error", 1)
				continue
			}
			ev, status, recognized := c.eventFromPayload(roomID, dm, now)
			if !recognized {
				continue
			}
			if dm.Method == methodControl {
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

// decodeFrame turns one raw downlink frame into decoded messages. With a
// platform Decoder it delegates entirely (the decoder owns gunzip+base64 /
// JSON and supplies its own ack token); otherwise it runs the douyin protobuf
// path and returns the response cursor + internal_ext ack token.
func (c *Config) decodeFrame(raw []byte) (cursor string, ackToken string, decoded []Decoded, err error) {
	if c.Decoder != nil {
		msgs, derr := c.Decoder.Decode(raw)
		return "", c.Decoder.AckToken(), msgs, derr
	}
	body, err := maybeGunzip(raw)
	if err != nil {
		return "", "", nil, fmt.Errorf("live: decompress: %w", err)
	}
	resp, err := DecodeResponse(body)
	if err != nil {
		return "", "", nil, fmt.Errorf("live: response decode: %w", err)
	}
	out := make([]Decoded, 0, len(resp.Messages))
	for _, msg := range resp.Messages {
		method, payload, ok := DecodeMessage(msg)
		out = append(out, Decoded{Method: method, Payload: payload, OK: ok})
	}
	return resp.Cursor, resp.InternalExt, out, nil
}

// sendAckWith writes an ack PushFrame carrying the given token.
func (c *Config) sendAckWith(conn *wsutil.Conn, token string) {
	var payload []byte
	if token != "" {
		payload = []byte(token)
	}
	if err := conn.WriteBinary(EncodePushFrame(PushFrame{PayloadType: "ack", Payload: payload})); err != nil {
		return
	}
	c.inc("live.ack", 1)
}

// eventFromPayload maps a decoded message to a LiveEvent. With a platform
// Decoder the method name is already the platform method (e.g. SCWebFeedPush)
// and we dispatch via the decoder's own Event mapping (control status 0);
// otherwise the douyin path, which returns the control status for control
// messages.
func (c *Config) eventFromPayload(roomID string, dm Decoded, now int64) (ev model.LiveEvent, status int64, recognized bool) {
	if c.Decoder != nil {
		ev, recognized = c.Decoder.Event(roomID, dm.Method, dm.Payload, now)
		return ev, 0, recognized
	}
	return EventFromMessage(roomID, dm.Method, dm.Payload, now)
}

// sendAckToken sends an ack using the Decoder's token (kuaishou: none).
func (c *Config) sendAckToken(conn *wsutil.Conn) {
	if c.Decoder != nil {
		// Kuaishou uses no internal_ext ack token; nothing to echo.
		return
	}
	// douyin ack is sent in decodeFrame path via sendAck; kept for compat.
}

// hbLoop sends a heartbeat PushFrame every interval until ctx is done or a
// write fails (a dead socket also surfaces in the read loop).
func hbLoop(ctx context.Context, conn *wsutil.Conn, c *Config, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	// Kuaishou uses a JSON heartbeat; douyin uses a protobuf PushFrame. The
	// Decoder supplies the right bytes when present.
	var frame []byte
	if c.Decoder != nil {
		frame = c.Decoder.Heartbeat()
	} else {
		frame = EncodePushFrame(PushFrame{PayloadType: "hb"})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if len(frame) == 0 {
				continue
			}
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

// defaultWSSEndpoint is the built-in fallback im-push endpoint (douyin's
// historic webcast host). The canonical source is the meta contract's
// transport_ws.wss_host; this constant only serves contracts that predate
// the field. MEDIAMON_LIVE_WSS_ENDPOINT (test-only escape hatch, never set
// in production) redirects the endpoint to a local server; wsutil accepts
// http:// as a ws:// alias.
const defaultWSSEndpoint = "wss://webcast100-ws-web-lq.douyin.com"

func wssEndpointRoot() string {
	if e := os.Getenv("MEDIAMON_LIVE_WSS_ENDPOINT"); e != "" {
		return strings.TrimRight(e, "/")
	}
	return defaultWSSEndpoint
}

// buildWSSURL assembles the wss dial URL from the platform meta contract's
// transport_ws section (host/path/fixed params/runtime-param names), then
// appends "&signature=" with the SignFn result last (the signature covers
// the params the SignFn chooses from urlQuery). Env override
// MEDIAMON_LIVE_WSS_ENDPOINT replaces scheme://host, matching the page
// endpoint pattern. Fail-closed when the section or its path is missing;
// a missing wss_host falls back to the built-in default endpoint (see
// wssEndpointRoot).
func (c *Config) buildWSSURL(roomID, cursor string) (string, error) {
	metaName := c.metaName()
	meta, ok := c.Registry.Get(metaName)
	if !ok {
		return "", fmt.Errorf("live: contract %q not registered", metaName)
	}
	tws := meta.TransportWS
	if tws == nil || tws.Path == "" {
		return "", fmt.Errorf("live: contract %q declares no transport_ws (path)", metaName)
	}
	root := wssEndpointRoot()
	if tws.WSSHost != "" {
		// The contract's wss_host is canonical; tolerate a scheme-qualified
		// value (ws:// or wss://) as well as a bare host.
		host := strings.TrimPrefix(strings.TrimPrefix(tws.WSSHost, "wss://"), "ws://")
		root = "wss://" + host
		if e := os.Getenv("MEDIAMON_LIVE_WSS_ENDPOINT"); e != "" {
			root = strings.TrimRight(e, "/")
		}
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
			return "", fmt.Errorf("live: contract %q declares unknown runtime param %q", metaName, rp)
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
// HTTP client and an Origin derived from the meta contract base_url (no
// hardcoded URL here).
func wssHeaders(hc *httpclient.Client, reg *contracts.Registry, metaName string) http.Header {
	h := http.Header{}
	if hc != nil {
		h.Set("User-Agent", hc.UA())
	}
	if origin := pageOrigin(reg, metaName); origin != "" {
		h.Set("Origin", origin)
	}
	return h
}

// pageOrigin derives the wss Origin header from the meta contract base_url
// (scheme://host).
func pageOrigin(reg *contracts.Registry, metaName string) string {
	c, ok := reg.Get(metaName)
	if !ok {
		return ""
	}
	u, err := url.Parse(c.Transport.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// defaultRoomHosts is the built-in fallback room-URL host whitelist
// (douyin's historic live hosts). The canonical source is the meta
// contract's transport base_url host + alt_hosts; these constants only
// serve contracts that declare no alt_hosts.
var defaultRoomHosts = []string{"live.douyin.com", "www.live.douyin.com"}

// normalizeRoomURL validates an input room URL and normalizes it to the
// canonical {base_url}/{room_web} form declared by the platform meta
// contract (transport base_url + path placeholder; accepted hosts come from
// the contract's base_url host and alt_hosts).
func normalizeRoomURL(reg *contracts.Registry, metaName, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("live: room url is required")
	}
	c, ok := reg.Get(metaName)
	if !ok {
		return "", fmt.Errorf("live: contract %q not registered", metaName)
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
	// Accepted hosts: the contract's transport base_url host plus its
	// declared alt_hosts. Contracts without alt_hosts fall back to the
	// built-in douyin aliases (defaultRoomHosts) for compatibility.
	contractHost := ""
	if cu, err := url.Parse(c.Transport.BaseURL); err == nil {
		contractHost = strings.ToLower(cu.Hostname())
	}
	allowed := map[string]bool{contractHost: true}
	if len(c.Transport.AltHosts) > 0 {
		for _, h := range c.Transport.AltHosts {
			allowed[strings.ToLower(h)] = true
		}
	} else {
		for _, h := range defaultRoomHosts {
			allowed[h] = true
		}
	}
	want := strings.ToLower(pu.Hostname())
	if !allowed[want] {
		return "", fmt.Errorf("live: unsupported room url host %q (want %q)", pu.Hostname(), contractHost)
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
// locator semantics declared by the meta contract's binding field "room_id".
func fetchRoomID(ctx context.Context, hc *httpclient.Client, reg *contracts.Registry, metaName, pageURL string) (string, error) {
	c, ok := reg.Get(metaName)
	if !ok {
		return "", fmt.Errorf("live: contract %q not registered", metaName)
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
