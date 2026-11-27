package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tvbox-aggregator/internal/model"
)

// TestRunEndToEnd 端到端全链路验证：
// 模拟采集站（测速成功）、模拟封锁站（403）、模拟爬虫站（免测）、
// 白名单源（存活探测），跑完整 run() 流程并断言四个产物。
func TestRunEndToEnd(t *testing.T) {
	segment := bytes.Repeat([]byte{1}, 1<<20) // 1MB 分片
	playlist := "#EXTM3U\n#EXT-X-VERSION:3\n" +
		"#EXTINF:10.0,\n/seg0.ts\n#EXTINF:10.0,\n/seg1.ts\n" +
		"#EXTINF:10.0,\n/seg2.ts\n#EXTINF:10.0,\n/seg3.ts\n#EXT-X-ENDLIST\n"

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "provide/vod"):
			// 苹果 CMS 兼容的采集站 API
			io.WriteString(w, fmt.Sprintf(
				`{"code":1,"msg":"ok","list":[{"vod_name":"测试影片","vod_play_url":"第1集$%s/index.m3u8"}]}`,
				srv.URL))
		case r.URL.Path == "/index.m3u8":
			io.WriteString(w, playlist)
		case r.URL.Path == "/x.jar":
			io.WriteString(w, "JAR")
		case strings.HasPrefix(r.URL.Path, "/seg"):
			w.Write(segment)
		case r.URL.Path == "/home":
			io.WriteString(w, "OK") // 白名单存活探测目标
		case r.URL.Path == "/sub.json":
			// 订阅式源配置：含一个 type 3 爬虫站与全局 spider
			io.WriteString(w, `{"spider":"./x.jar","sites":[{"key":"sk","name":"订阅动漫站","type":3,"api":"csp_DouDouGuard"}]}`)
		case r.URL.Path == "/":
			io.WriteString(w, "OK") // 爬虫站延迟探测目标
		case r.URL.Path == "/forbidden":
			w.WriteHeader(http.StatusForbidden) // 模拟海外封锁
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	sourcesYAML := filepath.Join(dir, "sources.yaml")
	whitelistYAML := filepath.Join(dir, "whitelist.yaml")
	outDir := filepath.Join(dir, "dist")

	sourcesContent := fmt.Sprintf(`top_n: 3
sources:
  - name: "模拟影视站"
    url: "%s/api.php/provide/vod"
    type: collect
  - name: "模拟剧集站"
    url: "%s/forbidden"
    type: collect
  - name: "模拟订阅源"
    url: "%s/sub.json"
    type: subscription
`, srv.URL, srv.URL, srv.URL)

	whitelistContent := fmt.Sprintf(`whitelist:
  - name: "模拟国内影视"
    url: "%s/home"
`, srv.URL)

	if err := os.WriteFile(sourcesYAML, []byte(sourcesContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(whitelistYAML, []byte(whitelistContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// jar 本地化重写地址
	// 发现配置不存在 → 自动发现禁用，走纯人工源池路径
	if err := run(sourcesYAML, whitelistYAML,
		filepath.Join(dir, "discover.yaml"), filepath.Join(dir, "discovered.yaml"),
		outDir, "https://cdn.example.com/jar"); err != nil {
		t.Fatalf("run 失败: %v", err)
	}

	// ---- 断言 tvbox.json（单一订阅版）：清洗 + 白名单置顶 + 速度排序 + 订阅提取爬虫保底 ----
	sub := loadConfig(t, filepath.Join(outDir, "tvbox.json"))
	if len(sub.Sites) != 3 {
		t.Fatalf("订阅版期望 3 个站点（top_n=3），实际 %d", len(sub.Sites))
	}
	// 首位：白名单源（国内直连置顶）
	if !strings.Contains(sub.Sites[0].Name, "[国内直连]") {
		t.Errorf("订阅版首位应为白名单源，实际 name=%q", sub.Sites[0].Name)
	}
	// 次位：测速成功源（带速度标签）
	if !strings.Contains(sub.Sites[1].Name, "🚀") {
		t.Errorf("订阅版次位应为测速成功源，实际 name=%q", sub.Sites[1].Name)
	}
	// 末位：订阅提取的爬虫站保底，上游 key 保留，jar 解析为绝对地址且写入客户端读取的字段
	spider := sub.Sites[2]
	if !strings.Contains(spider.Name, "订阅动漫站") {
		t.Errorf("订阅版末位应为订阅提取的爬虫站，实际 name=%q", spider.Name)
	}
	if spider.Type == nil || spider.Type.Value != 3 {
		t.Errorf("订阅提取的 csp 源应保留原始 type=3，实际 %+v", spider.Type)
	}
	if spider.Key != "sk" {
		t.Errorf("订阅提取的站点 key 必须保留，实际 %q", spider.Key)
	}
	if spider.Jar != srv.URL+"/x.jar" {
		t.Errorf("订阅提取 jar 应解析为绝对地址且不重写，实际 %q", spider.Jar)
	}
	// 顶层 spider 必须存在，供未声明 jar 的 csp 站点加载
	if sub.Spider != srv.URL+"/x.jar" {
		t.Errorf("顶层 spider 应设为全局 jar，实际 %q", sub.Spider)
	}
	if sub.Lives == nil || sub.Parses == nil {
		t.Error("顶层 lives/parses 应输出为空数组而非 null")
	}
	// 严格客户端兼容性校验，防止再次产出 TVBox 无法加载的配置
	validateTVBoxCompatibility(t, &sub)
	// 清洗：封锁站不应出现
	for _, s := range sub.Sites {
		if strings.Contains(s.Name, "封锁") {
			t.Errorf("清洗后不应包含封锁站: %q", s.Name)
		}
	}

	// ---- 断言不再产出多版本文件 ----
	for _, name := range []string{"tvbox_full.json", "tvbox_multi.json"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err == nil {
			t.Errorf("不应再产出 %s", name)
		}
	}

	// ---- 断言 status.json 诊断 ----
	statusData, err := os.ReadFile(filepath.Join(outDir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Summary struct {
			Total     int `json:"total"`
			OK        int `json:"ok"`
			Blocked   int `json:"blocked"`
			Spider    int `json:"spider"`
			Whitelist int `json:"whitelist"`
		} `json:"summary"`
		Sources       []*model.TestResult       `json:"sources"`
		Whitelist     []model.WhitelistProbe    `json:"whitelist"`
		Subscriptions []model.SubscriptionProbe `json:"subscriptions"`
	}
	if err := json.Unmarshal(statusData, &rep); err != nil {
		t.Fatalf("status.json 解析失败: %v", err)
	}
	if rep.Summary.Total != 3 || rep.Summary.OK != 1 || rep.Summary.Blocked != 1 ||
		rep.Summary.Spider != 1 || rep.Summary.Whitelist != 1 {
		t.Errorf("status 摘要错误: %+v", rep.Summary)
	}
	if len(rep.Whitelist) != 1 || !rep.Whitelist[0].Alive {
		t.Errorf("白名单应探测存活: %+v", rep.Whitelist)
	}
	// 订阅提取的爬虫站延迟探测结果应纳入诊断（含 TTFB）
	var spiderResult *model.TestResult
	for _, r := range rep.Sources {
		if r.URL == srv.URL+"/sub.json" {
			spiderResult = r
			break
		}
	}
	if spiderResult == nil {
		t.Error("status.json 应包含爬虫站延迟探测结果")
	} else if spiderResult.TTFBMs <= 0 {
		t.Errorf("爬虫站探测应记录 TTFB，实际 %d", spiderResult.TTFBMs)
	}
	// 订阅式源抓取诊断应纳入 status.json
	var subProbe model.SubscriptionProbe
	foundSub := false
	for _, p := range rep.Subscriptions {
		if p.Name == "模拟订阅源" {
			subProbe = p
			foundSub = true
			break
		}
	}
	if !foundSub {
		t.Error("status.json 应包含订阅抓取诊断")
	} else if subProbe.Status != model.StatusOK || subProbe.Sites != 1 {
		t.Errorf("订阅诊断错误: %+v", subProbe)
	}
}

// validateTVBoxCompatibility 按 TVBox/影视仓的必填字段校验产物：
// key 必填且唯一；type 必填；api 必填；type=3 需 jar 或顶层 spider。
func validateTVBoxCompatibility(t *testing.T, cfg *model.TVBoxConfig) {
	t.Helper()
	if len(cfg.Sites) == 0 {
		t.Fatal("产物站点为空，客户端将没有线路")
	}
	keys := make(map[string]bool, len(cfg.Sites))
	for i, s := range cfg.Sites {
		if s.Key == "" {
			t.Errorf("第 %d 个站点缺少 key: %+v", i, s)
		}
		if keys[s.Key] {
			t.Errorf("重复 key %q", s.Key)
		}
		keys[s.Key] = true
		if s.Type == nil || !s.Type.Set {
			t.Errorf("第 %d 个站点缺少 type", i)
		}
		if s.API == "" {
			t.Errorf("第 %d 个站点缺少 api", i)
		}
		if s.Type != nil && s.Type.Value == 3 && s.Jar == "" && cfg.Spider == "" {
			t.Errorf("type=3 站点 %q 无 jar 且顶层无 spider", s.Name)
		}
	}
}

func loadConfig(t *testing.T, path string) model.TVBoxConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	var c model.TVBoxConfig
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
	return c
}
