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
		case strings.HasPrefix(r.URL.Path, "/seg"):
			w.Write(segment)
		case r.URL.Path == "/home":
			io.WriteString(w, "OK") // 白名单存活探测目标
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
  - name: "模拟采集站"
    url: "%s/api.php/provide/vod"
    type: collect
  - name: "模拟封锁站"
    url: "%s/forbidden"
    type: collect
  - name: "模拟爬虫站"
    url: "%s/"
    type: spider
    spider: "jar/spider.jar"
`, srv.URL, srv.URL, srv.URL)

	whitelistContent := fmt.Sprintf(`whitelist:
  - name: "模拟国内源"
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

	// ---- 断言 tvbox.json（单一订阅版）：清洗 + 白名单置顶 + 速度排序 + 爬虫保底 ----
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
	// 末位：爬虫站保底保留，JAR 已重写为本地地址
	spider := sub.Sites[2]
	if !strings.Contains(spider.Name, "爬虫") {
		t.Errorf("订阅版末位应为爬虫站，实际 name=%q", spider.Name)
	}
	if spider.Type == nil || spider.Type.Value != 1 {
		t.Error("爬虫站 type 应为 1")
	}
	if spider.Spider != "https://cdn.example.com/jar/spider.jar" {
		t.Errorf("spider 应被重写为本地地址，实际 %q", spider.Spider)
	}
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
		Sources   []*model.TestResult     `json:"sources"`
		Whitelist []model.WhitelistProbe `json:"whitelist"`
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
	// 爬虫站延迟探测结果应纳入诊断（含 TTFB）
	var spiderResult *model.TestResult
	for _, r := range rep.Sources {
		if r.URL == srv.URL+"/" {
			spiderResult = r
			break
		}
	}
	if spiderResult == nil {
		t.Error("status.json 应包含爬虫站延迟探测结果")
	} else if spiderResult.TTFBMs <= 0 {
		t.Errorf("爬虫站探测应记录 TTFB，实际 %d", spiderResult.TTFBMs)
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
