// browserhdr.go — browser-grade default request headers for the xiaohongshu
// web surface (silent-scraping round 2, report item 2 / B1). Shape mirrors
// the recorded corpus (XHR from www.xiaohongshu.com to the edith API host):
// origin+referer, accept/accept-language, sec-fetch same-site (the API host
// is cross-origin from the page), plus the engine-derived sec-ch-ua family
// (UA-consistent, computed in internal/collect). No cookie values here.
package xhs

// BrowserHeaders returns the default browser header set merged under every
// xhs request (contract transport.headers still override these values).
func BrowserHeaders() map[string]string {
	return map[string]string{
		"Accept":          "application/json, text/plain, */*",
		"Accept-Language": "zh-CN,zh;q=0.9",
		"Accept-Encoding": "gzip, deflate",
		"Origin":          "https://www.xiaohongshu.com",
		"Referer":         "https://www.xiaohongshu.com/",
		"Cache-Control":   "no-cache",
		"Pragma":          "no-cache",
		"Priority":        "u=1, i",
		"Sec-Fetch-Dest":  "empty",
		"Sec-Fetch-Mode":  "cors",
		"Sec-Fetch-Site":  "same-site",
	}
}
