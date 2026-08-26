package live

import (
	"crypto/md5"
	"encoding/hex"
	"sort"
	"strings"
)

// MD5StubSigner is the explicitly non-production signature placeholder shared
// by the CLI live monitor (cmd/mediactl) and the MCP live tools
// (cmd/mediad-mcp) for local development (docs/HARDENING.md logic-exclusion
// model): the sorted parameter k=v pairs are concatenated and MD5-hexed.
// urlQuery is part of the SignFn shape but unused by this stub. Production
// paths dial internal/signclient instead.
func MD5StubSigner(urlQuery string, params map[string]string) (string, error) {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(params[k])
	}
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}
