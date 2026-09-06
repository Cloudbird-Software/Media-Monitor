// browserhdr.go — browser-grade default request headers for the kuaishou web
// surface (silent-scraping round 2, report item 2 / B1). Shape mirrors the
// recorded corpus (XHR to www.kuaishou.com/rest/*): accept/accept-language/
// referer/sec-fetch-*/cache-control + the engine-derived sec-ch-ua family
// (UA-consistent, computed in internal/collect). No cookie values here.
package kuaishou

// BrowserHeaders returns the default browser header set merged under every
// kuaishou request (contract transport.headers still override these values).
func BrowserHeaders() map[string]string {
	return map[string]string{
		"Accept":          "application/json",
		"Accept-Language": "zh-CN,zh;q=0.9",
		"Accept-Encoding": "gzip, deflate",
		"Referer":         "https://www.kuaishou.com/",
		"Origin":          "https://www.kuaishou.com",
		"Cache-Control":   "no-cache",
		"Pragma":          "no-cache",
		"Connection":      "keep-alive",
		"Sec-Fetch-Dest":  "empty",
		"Sec-Fetch-Mode":  "cors",
		"Sec-Fetch-Site":  "same-origin",
	}
}
