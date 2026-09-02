// browserhdr.go — browser-grade default request headers for the douyin web
// surface (silent-scraping round 2, report item 2 / B1: the collector used
// to send 4-5 headers where the human baseline sends 19-28). The shape
// mirrors the recorded corpus (XHR to www.douyin.com/aweme/v1/web/*):
// accept/accept-language/referer/sec-fetch-*/priority/cache-control, with the
// UA-consistent sec-ch-ua family derived by the engine from the pinned UA
// (those live in internal/collect — they must match the UA, not a constant).
// No cookie or credential values live here.
package douyin

// BrowserHeaders returns the default browser header set merged under every
// douyin request (contract transport.headers still override these values).
// Mirrors the corpus XHR shape (accept/accept-language/referer/priority/
// cache-control+pragma/sec-fetch same-origin); the sec-ch-ua family rides on
// top, derived from the pinned UA by the engine.
func BrowserHeaders() map[string]string {
	return map[string]string{
		"Accept":           "application/json, text/plain, */*",
		"Accept-Language":  "zh-CN,zh;q=0.9",
		"Accept-Encoding":  "gzip, deflate",
		"Referer":          "https://www.douyin.com/",
		"Cache-Control":    "no-cache",
		"Pragma":           "no-cache",
		"Priority":         "u=1, i",
		"Sec-Fetch-Dest":   "empty",
		"Sec-Fetch-Mode":   "cors",
		"Sec-Fetch-Site":   "same-origin",
	}
}
