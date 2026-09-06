// impersonate_test.go — ADR-0100 指纹拟态验证（env 门控）。
//
//	MEDIAMON_FINGERPRINT_CHECK=1 go test ./internal/httpclient -run TestImpersonateFingerprint -v
//
// 对 https://tls.peet.ws/api/all（公开指纹检测服务）分别以 stdlib 与
// MEDIAMON_TLS_IMPERSONATE=chrome152 发起请求，断言：
//  1. 两条路径均可达且返回 JA3/JA4；
//  2. 两路径 JA3 不同（拟态生效）；
//  3. 拟态路径 JA4 以 "t13d" 开头（TLS1.3 Chrome 形态）且与 stdlib 不同。
package httpclient

import (
	"encoding/json"
	"os"
	"testing"
)

func fingerprintGate(t *testing.T) {
	t.Helper()
	if os.Getenv("MEDIAMON_FINGERPRINT_CHECK") != "1" {
		t.Skip("fingerprint check: set MEDIAMON_FINGERPRINT_CHECK=1（访问公开检测服务 tls.peet.ws）")
	}
}

type peetResp struct {
	TLS struct {
		JA3  string `json:"ja3"`
		JA3H string `json:"ja3_hash"`
		JA4  string `json:"ja4"`
	} `json:"tls"`
	H2 struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"http2"`
	UserA string `json:"user_agent"`
}

func peet(t *testing.T, impersonate bool) peetResp {
	t.Helper()
	_ = os.Unsetenv("MEDIAMON_TLS_IMPERSONATE")
	if impersonate {
		_ = os.Setenv("MEDIAMON_TLS_IMPERSONATE", "chrome152")
	} else {
		_ = os.Unsetenv("MEDIAMON_TLS_IMPERSONATE")
	}
	c := New(Config{Timeout: 30000000000}) // 30s
	status, body, err := c.Do(t.Context(), "GET", "https://tls.peet.ws/api/all", nil, nil)
	if err != nil {
		t.Fatalf("peet.ws request failed: %v", err)
	}
	if status != 200 {
		t.Fatalf("peet.ws status %d", status)
	}
	var pr peetResp
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatalf("peet.ws parse: %v (body head %.80s)", err, body)
	}
	return pr
}

func TestImpersonateFingerprint(t *testing.T) {
	fingerprintGate(t)
	std := peet(t, false)
	imp := peet(t, true)
	t.Logf("stdlib : ja3_hash=%s ja4=%s h2=%s", std.TLS.JA3H, std.TLS.JA4, std.H2.Fingerprint)
	t.Logf("chrome152: ja3_hash=%s ja4=%s h2=%s ua=%s", imp.TLS.JA3H, imp.TLS.JA4, imp.H2.Fingerprint, imp.UserA)
	if std.TLS.JA3H == "" || imp.TLS.JA3H == "" {
		t.Fatal("peet.ws 未返回 ja3_hash（字段结构变更？）")
	}
	if std.TLS.JA3H == imp.TLS.JA3H {
		t.Fatal("impersonation had no effect on JA3 (指纹未变)")
	}
	if imp.TLS.JA4 == "" || len(imp.TLS.JA4) < 4 || imp.TLS.JA4[:4] != "t13d" {
		t.Fatalf("impersonate JA4 not TLS1.3 Chrome form: %q", imp.TLS.JA4)
	}
	if std.TLS.JA4 == imp.TLS.JA4 {
		t.Fatal("JA4 identical between stdlib and impersonate")
	}
}
