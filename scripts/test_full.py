"""完整端到端回归测试"""
import urllib.request, json, sys

BASE = 'http://localhost:8080'
PASS = 0
FAIL = 0

def check(name, cond, detail=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print(f"  ✅ {name}")
    else:
        FAIL += 1
        print(f"  ❌ {name}  {detail}")

# 1. healthz
try:
    r = urllib.request.urlopen(BASE + '/healthz', timeout=3).read().decode()
    check("healthz 200", '"ok"' in r, r)
except Exception as e:
    check("healthz 200", False, str(e))
    sys.exit(1)

# 2. login
try:
    req = urllib.request.Request(BASE + '/api/v1/login',
        data=json.dumps({'user_id': 'alice'}).encode(),
        headers={'Content-Type': 'application/json'})
    token = json.loads(urllib.request.urlopen(req, timeout=3).read())['token']
    check("login JWT", len(token) > 50, f"len={len(token)}")
except Exception as e:
    check("login JWT", False, str(e))
    sys.exit(1)

auth = {'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'}

# 3. 鉴权拦截（无 token 应该 401）
try:
    urllib.request.urlopen(BASE + '/api/v1/stats/daily', timeout=3)
    check("无 token 被拒", False, "应该 401 但没被拒")
except urllib.error.HTTPError as e:
    check("无 token 被拒", e.code == 401, f"code={e.code}")
except Exception as e:
    check("无 token 被拒", False, str(e))

# 4. 同步 chat.completions
try:
    body = json.dumps({'model': 'mock-gpt', 'messages': [{'role': 'user', 'content': 'hi'}], 'stream': False}).encode()
    req = urllib.request.Request(BASE + '/api/v1/chat/completions', data=body, headers=auth)
    r = json.loads(urllib.request.urlopen(req, timeout=10).read())
    check("同步 chat", 'content' in r, str(r)[:80])
    check("有 finish_reason", r.get('finish_reason') == 'stop')
    check("有 usage", 'usage' in r)
except Exception as e:
    check("同步 chat", False, str(e))

# 5. 第二次相同请求 → 缓存命中
try:
    req = urllib.request.Request(BASE + '/api/v1/chat/completions', data=body, headers=auth)
    r2 = json.loads(urllib.request.urlopen(req, timeout=10).read())
    check("第二次 chat 也返回正常", 'content' in r2)
except Exception as e:
    check("第二次 chat", False, str(e))

# 6. SSE 流式
try:
    body = json.dumps({'model': 'mock-gpt', 'messages': [{'role': 'user', 'content': 'stream me'}], 'stream': True}).encode()
    req = urllib.request.Request(BASE + '/api/v1/chat/completions', data=body, headers=auth)
    resp = urllib.request.urlopen(req, timeout=15)
    ct = resp.headers.get('Content-Type', '')
    raw = resp.read().decode('utf-8', errors='replace')
    lines = [l for l in raw.split('\n') if l.startswith('data:')]
    check("SSE Content-Type", 'text/event-stream' in ct, ct)
    check("SSE 有分片", len(lines) >= 3, f"分片数={len(lines)}")
    check("SSE 有 [DONE]", any('[DONE]' in l for l in lines))
except Exception as e:
    check("SSE 流式", False, str(e))

# 7. metrics
try:
    m = urllib.request.urlopen(BASE + '/metrics', timeout=3).read().decode()
    for key in ['provider_calls_total', 'provider_failures_total', 'cache_hits_total',
                 'cache_misses_total', 'http_requests_total', 'circuit_breaker_open_total']:
        check(f"metrics 包含 {key}", key in m)
except Exception as e:
    check("metrics", False, str(e))

# 8. stats/daily
try:
    req = urllib.request.Request(BASE + '/api/v1/stats/daily', headers=auth)
    r = json.loads(urllib.request.urlopen(req, timeout=3).read())
    check("stats/daily", 'date' in r, str(r)[:80])
except Exception as e:
    check("stats/daily", False, str(e))

print(f"\n{'='*40}")
print(f"  结果: {PASS} PASS  {FAIL} FAIL  共 {PASS+FAIL}")
print(f"{'='*40}")
sys.exit(0 if FAIL == 0 else 1)
