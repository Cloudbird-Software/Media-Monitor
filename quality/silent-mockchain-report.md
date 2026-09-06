# Silent-scraping mock-chain validation (loopback)

- contract: douyin-search shape vs httptest loopback, 25 pages, limit=500 (would have been count=500 passthrough before)
- pacing under test: median=300ms sigma=1.0 (scaled; production default 1500ms/1.0 — see TestLognormalSleepDistribution: p50=1.5s p90≈5.4s p99≈15s max≤30s)

| metric | value | bar | verdict |
|---|---|---|---|
| interval p50 | 320 ms | ≥150ms (pacing engaged) | PASS |
| interval p90 | 1305 ms (p90/p50=4.07) | ≥1.8×p50 (heavy tail) | PASS |
| interval max | 2696 ms | ≤3000ms cap | PASS |
| interval CV | 1.11 | — (report) | info |
| min header count | 15 | ≥15 | PASS |
| count param | all "20" | ≤20, no limit passthrough | PASS |
| UA stability | 1 UA across 25 reqs | constant per session | PASS |
| sec-ch-ua family | Chrome brand on every request | UA-consistent | PASS |
| referer/cookie | present on every request | — | PASS |

UA pinned for the session: Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36
