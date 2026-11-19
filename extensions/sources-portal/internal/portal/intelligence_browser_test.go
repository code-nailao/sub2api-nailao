package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Only compiled into the explicit loopback test binary, never the shipped app.
func intelligenceBrowserFixture(t *testing.T) *Server {
	t.Helper()
	s, native := intelligenceAPIFixture(t)
	s.Intelligence.provider = &intelligenceBrowserProvider{}
	s.Service.Intelligence = s.Intelligence
	now := time.Now().UTC()
	names := []string{"【不降智】Codex Plus", "【不降智】Codex Pro「自用推荐」", "【不降智】Codex Pro Max"}
	if raw := os.Getenv("PORTAL_INTELLIGENCE_FIXTURE_PLANS"); raw != "" {
		count, err := strconv.Atoi(raw)
		if err != nil || count < 3 || count > 32 {
			t.Fatal("fixture plan count must be between 3 and 32")
		}
		for len(names) < count {
			names = append(names, fmt.Sprintf("演示检测分组 %02d", len(names)+1))
		}
	}
	native.groups = nil
	for index, name := range names {
		groupID := int64(17 + index)
		expires := now.Add(30 * 24 * time.Hour)
		native.groups = append(native.groups, StudioGroup{ID: groupID, Name: name, Platform: "openai", Status: "active"})
		key := intelligenceNativeKey{ID: int64(200 + index), UserID: 9, GroupID: groupID, Name: "Fixture monitor", Key: "sk-fixture-monitor-only", Status: "active", Quota: 200, DailyLimit: 20, ExpiresAt: &expires}
		native.keys[key.ID] = key
		plan := IntelligencePlan{Visibility: "authenticated", CreativeEveryFive: true, DrawingSequence: 4, ID: fmt.Sprintf("%032x", index+1), Revision: 1, GroupID: groupID, Name: name, Model: intelligenceModel, Effort: "medium", CandySeconds: 300, PelicanSeconds: 1200, NextCandy: now.Add(5 * time.Minute), NextPelican: now.Add(20 * time.Minute), KeyID: key.ID, OwnerID: 9, KeyStatus: "active", DailyLimit: 20, Quota: 200, ExpiresAt: &expires}
		ciphertext, err := s.Intelligence.Store.encrypt(plan, key.Key)
		if err != nil {
			t.Fatal(err)
		}
		rows := map[string]intelligenceRunRecord{}
		drawings := 6
		if os.Getenv("PORTAL_INTELLIGENCE_FIXTURE_DRAWINGS") == "20" {
			drawings = 20
		}
		for i := 0; i < 60+drawings; i++ {
			kind, verdict := "candy", "passed"
			result := IntelligenceResult{Answer: "最少取出 21 个。<script>not executable</script>"}
			if i%17 == 0 {
				verdict = "incorrect"
			}
			if i >= 60 {
				kind, verdict = "pelican", "normal"
				if i%5 == 4 {
					kind = "creative"
				}
				result = IntelligenceResult{HTML: intelligenceBrowserHTML}
				if dir := os.Getenv("PORTAL_INTELLIGENCE_FIXTURE_HTML_DIR"); dir != "" {
					file := filepath.Join(dir, fmt.Sprintf("pelican-%d.html", []int{1, 4, 3}[i%3]))
					raw, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					result.HTML = string(raw)
				}
			}
			id := fmt.Sprintf("%032x", 1000+index*100+i)
			at := now.Add(time.Duration(i-66) * 5 * time.Minute)
			d := int64(12400)
			run := IntelligenceRun{ID: id, PlanID: plan.ID, Name: name, Benchmark: kind, Model: intelligenceModel, Effort: "medium", Status: "succeeded", Phase: "complete", Verdict: verdict, StartedAt: at, FinishedAt: &at, DurationMS: &d, HasResult: true, HasHTML: intelligenceDrawing(kind)}
			if err := s.Intelligence.Store.saveResult(id, result); err != nil {
				t.Fatal(err)
			}
			rows[id] = intelligenceRunRecord{Run: run, RemoteID: "private-fixture-id"}
		}
		if err := s.Intelligence.Store.change(func(st *intelligenceState) error {
			st.Plans[plan.ID] = intelligencePlanRecord{Plan: plan, Secret: ciphertext, Attempted: true, Operation: randomID()}
			for id, row := range rows {
				st.Runs[id] = row
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	native.groups = append(native.groups, StudioGroup{ID: int64(17 + len(names)), Name: "私有测试分组", Platform: "openai", Status: "active", IsExclusive: true})
	return s
}

const intelligenceBrowserHTML = `<!doctype html><html><head><style>
html,body{margin:0;background:#f4f2e9;font-family:system-ui}main{width:1280px;height:1100px;box-sizing:border-box;padding:60px;display:flex;flex-direction:column;align-items:center;justify-content:space-between;color:#345d58}h1{font-size:45px}svg{width:100%;height:700px}footer{font-size:25px}
</style></head><body><main><h1>海风，顺路。</h1><svg viewBox="0 0 1000 650"><rect width="1000" height="650" rx="30" fill="#cbe7dd"/><circle cx="800" cy="120" r="50" fill="#ffe8a0"/><path d="M0 450Q200 340 400 450T1000 450V650H0" fill="#c9d7a9"/><g fill="none" stroke="#365a59" stroke-width="10"><circle cx="340" cy="450" r="100"/><circle cx="680" cy="450" r="100"/><path d="m340 450 100-150 70 150H340l280-150 60 150M440 300h180"/></g><ellipse cx="500" cy="250" rx="90" ry="70" fill="#fffaf0" stroke="#365a59" stroke-width="5"/><path d="M550 270Q630 220 580 140Q620 80 660 140L610 250" fill="#fffaf0" stroke="#365a59" stroke-width="5"/><path d="m650 145 150 15-145 50Z" fill="#eeb956"/><circle cx="638" cy="137" r="6"/></svg><footer id="bottom-marker">完整作品底部 · 浏览器夹具</footer></main><script>document.documentElement.dataset.fixtureScript="ran"</script></body></html>`

type intelligenceBrowserProvider struct{}

func (*intelligenceBrowserProvider) Submit(_ context.Context, in intelligenceSubmission, operation string) (intelligenceRemoteResult, error) {
	out := intelligenceRemoteResult{ID: "fixture-task-" + operation, Status: "succeeded", Benchmark: in.Benchmark}
	out.Candy.Status, out.Candy.Answer = "passed", "21"
	out.Result.HTML, out.Assessment.Quality = intelligenceBrowserHTML, "normal"
	return out, nil
}
func (*intelligenceBrowserProvider) Poll(_ context.Context, id string) (intelligenceRemoteResult, error) {
	return intelligenceRemoteResult{ID: id, Status: "running"}, nil
}

func intelligenceFixtureHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture/intelligence-login" {
			// Local browser tests select the same synthetic identities as the API
			// fixtures. This handler is compiled only into the test binary.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
			_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>本地智商测试验收</title><style>body{font:16px system-ui;padding:32px}button{font:inherit;margin:8px;padding:12px}</style>
<h1>本地测试身份</h1><p>仅用于本地合成数据验收，不连接生产或模型服务。</p><button id="admin">测试管理员</button><button id="user">测试普通用户</button>
<script>for(const role of ['admin','user'])document.getElementById(role).onclick=()=>{localStorage.setItem('auth_token','fixture-'+role);location.href='/intelligence-tests?manage=1&theme=light'}</script>`))
			return
		}
		if r.URL.Path != "/fixture/intelligence-parent" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; frame-src 'self'")
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><style>body{margin:0;font:16px system-ui;background:#142030;color:white}header{height:60px;padding:10px;box-sizing:border-box}iframe{width:100%;height:calc(100vh - 60px);border:0}button{margin-left:10px}</style><header>主站标题：智商测试<button id="dark">暗色</button><button id="light">亮色</button><button id="teal">青色</button></header><iframe id="child" src="/intelligence-tests?manage=1&ui_mode=embedded"></iframe><script>
const f=document.getElementById('child');let mode='light',color='yellow';
function send(){f.contentWindow.postMessage({type:'sources:intelligence-appearance',mode,color},location.origin)}
for(const key of ['dark','light','teal'])document.getElementById(key).onclick=()=>{if(key==='teal')color=key;else mode=key;try{localStorage.setItem(key==='teal'?'theme-color':'theme',key)}catch{}send()};
f.onload=send;addEventListener('message',e=>{if(e.source===f.contentWindow&&e.origin===location.origin&&e.data?.type==='sources:intelligence-appearance-ready')send()});
</script>`))
	})
}

func TestIntelligenceResultNeverExposesPrivateRemoteID(t *testing.T) {
	s := intelligenceBrowserFixture(t)
	out := request(s, "GET", "/api/intelligence-tests/snapshot", "", "fixture-admin", "")
	if out.Code != 200 || strings.Contains(out.Body.String(), "private-fixture-id") || strings.Contains(out.Body.String(), "sk-fixture") {
		t.Fatal("browser snapshot leaked server-side credentials")
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(out.Body.Bytes(), &value) != nil || value["plans"] == nil {
		t.Fatal("invalid public snapshot")
	}
}
