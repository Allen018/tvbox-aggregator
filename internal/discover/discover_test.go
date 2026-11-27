package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"tvbox-aggregator/internal/fetcher"
	"tvbox-aggregator/internal/model"
)

const vodJSON = `{"code":1,"msg":"ok","list":[{"vod_name":"测试影片","vod_play_url":"第1集$http://127.0.0.1:1/1.m3u8"}]}`

func TestExtractURLs(t *testing.T) {
	text := `配置地址 http://a.com/motif/api.php/provide/vod 可用；备用 https://b.com/api.php/provide/vod。
	另有 <a href="http://c.com/">站点</a> 与重复 http://a.com/motif/api.php/provide/vod`
	urls := extractURLs(text)
	want := []string{
		"http://a.com/motif/api.php/provide/vod",
		"https://b.com/api.php/provide/vod",
		"http://c.com/",
	}
	if len(urls) != len(want) {
		t.Fatalf("期望 %d 个链接，实际 %d: %v", len(want), len(urls), urls)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Errorf("第 %d 个=%q，期望 %q", i, urls[i], want[i])
		}
	}
}

func TestCandidateURLs(t *testing.T) {
	// 完整 API 路径：直接探测
	got := candidateURLs("http://a.com/motif/api.php/provide/vod")
	if len(got) != 1 || got[0] != "http://a.com/motif/api.php/provide/vod" {
		t.Errorf("完整 API 路径应原样探测，实际 %v", got)
	}
	// 裸域名：生成标准端点组合
	got = candidateURLs("http://a.com/")
	want := []string{
		"http://a.com/motif/api.php/provide/vod",
		"http://a.com/api.php/provide/vod",
		"https://a.com/motif/api.php/provide/vod",
		"https://a.com/api.php/provide/vod",
	}
	if len(got) != len(want) {
		t.Fatalf("期望 %d 个候选，实际 %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个=%q，期望 %q", i, got[i], want[i])
		}
	}
	// 非法输入
	if got := candidateURLs("not a url"); got != nil {
		t.Errorf("非法输入应返回 nil，实际 %v", got)
	}
}

func TestSiteName(t *testing.T) {
	if got := siteName("http://api.wujinren.cn/motif/api.php/provide/vod"); got != "发现·api.wujinren.cn" {
		t.Errorf("站点名错误: %q", got)
	}
}

func TestValidate(t *testing.T) {
	// 标准端点返回合法 JSON → 验证通过
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "provide/vod") {
			io.WriteString(w, vodJSON)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	d := New(Config{}, fetcher.New(10*time.Second), "")
	host := strings.TrimPrefix(srv.URL, "http://")

	src := d.validate(context.Background(), "http://"+host+"/")
	if src == nil {
		t.Fatal("裸域名候选应验证通过")
	}
	if src.URL != srv.URL+"/motif/api.php/provide/vod" {
		t.Errorf("验证 URL 错误: %q", src.URL)
	}
	if src.Type != "collect" || src.Name == "" {
		t.Errorf("源字段错误: %+v", src)
	}

	// 返回 HTML 假页 → 验证不通过
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<!DOCTYPE html><html>假页</html>")
	}))
	defer bad.Close()
	badHost := strings.TrimPrefix(bad.URL, "http://")
	if src := d.validate(context.Background(), "http://"+badHost+"/motif/api.php/provide/vod"); src != nil {
		t.Error("HTML 假页不应验证通过")
	}
}

func TestGitHubChannelNoToken(t *testing.T) {
	d := New(Config{GitHub: GitHubConfig{Enabled: true}}, fetcher.New(10*time.Second), "")
	if _, err := d.githubChannel(context.Background()); err == nil {
		t.Error("无 Token 时应返回错误")
	}
}

func TestGitHubChannel(t *testing.T) {
	var gotAuth bool
	githubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/code" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization") != ""
		resp := map[string]any{
			"items": []map[string]any{
				{"text_matches": []map[string]any{
					{"fragment": "订阅地址 http://example-source.com/motif/api.php/provide/vod 可用"},
				}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer githubSrv.Close()

	cfg := Config{GitHub: GitHubConfig{
		Enabled: true,
		Queries: []string{"motif/api.php/provide/vod"},
		BaseURL: githubSrv.URL,
	}}
	d := New(cfg, fetcher.New(10*time.Second), "test-token")
	urls, err := d.githubChannel(context.Background())
	if err != nil {
		t.Fatalf("githubChannel 报错: %v", err)
	}
	if !gotAuth {
		t.Error("应携带 Authorization 头")
	}
	if len(urls) != 1 || urls[0] != "http://example-source.com/motif/api.php/provide/vod" {
		t.Errorf("提取链接错误: %v", urls)
	}
}

func TestIndexChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html><body>
			<a href="http://a.com/motif/api.php/provide/vod">源一</a>
			<a href="https://b.com/">源二</a>
		</body></html>`)
	}))
	defer srv.Close()

	d := New(Config{}, fetcher.New(10*time.Second), "")
	urls, err := d.indexChannel(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("indexChannel 报错: %v", err)
	}
	if len(urls) != 2 || urls[0] != "http://a.com/motif/api.php/provide/vod" || urls[1] != "https://b.com/" {
		t.Errorf("索引站链接提取错误: %v", urls)
	}
}

// TestRunDualChannel 双通道端到端：GitHub + 索引站收集候选，
// 验证、去重、排除已有源、上轮复验淘汰失效源。
func TestRunDualChannel(t *testing.T) {
	// 验证服务器：标准端点返回合法 JSON
	validSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "provide/vod") {
			io.WriteString(w, vodJSON)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer validSrv.Close()
	host := strings.TrimPrefix(validSrv.URL, "http://")
	validURL := validSrv.URL + "/motif/api.php/provide/vod"

	// GitHub API 模拟：返回含有效源、死链、已有源的片段
	githubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"items": []map[string]any{
				{"text_matches": []map[string]any{
					{"fragment": fmt.Sprintf(
						"订阅 %s 可用；死链 http://127.0.0.1:1/motif/api.php/provide/vod；已有 http://existing.example/motif/api.php/provide/vod",
						validURL)},
				}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer githubSrv.Close()

	// 索引站模拟：返回裸域名链接
	indexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fmt.Sprintf(`<a href="%s/">示例站</a>`, validSrv.URL))
	}))
	defer indexSrv.Close()

	cfg := Config{
		Enabled:    true,
		MaxSources: 10,
		GitHub: GitHubConfig{
			Enabled: true,
			Queries: []string{"test"},
			BaseURL: githubSrv.URL,
		},
		IndexSites: []IndexSite{{URL: indexSrv.URL}},
	}
	d := New(cfg, fetcher.New(10*time.Second), "test-token")

	existing := map[string]bool{
		"http://existing.example/motif/api.php/provide/vod": true,
	}
	prev := []model.SourceConfig{
		{Name: "上轮有效源", URL: validURL},                                       // 复验应保留
		{Name: "上轮失效源", URL: "http://127.0.0.1:1/motif/api.php/provide/vod"}, // 复验应淘汰
	}

	out, err := d.Run(context.Background(), existing, prev)
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("期望 1 个发现源（双通道+上轮复验去重），实际 %d: %+v", len(out), out)
	}
	if out[0].URL != validURL {
		t.Errorf("发现源 URL 错误: %q", out[0].URL)
	}
	if out[0].Name != "发现·"+host {
		t.Errorf("发现源名称错误: %q", out[0].Name)
	}
}

func TestClbugChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html>
			<a data-url="http://a.com/sub.json">A</a>
			<a data-url="http://b.com/x.json">B</a>
			<a data-url="http://a.com/sub.json">dup</a>
			<a data-url="#comment">C</a>
		</html>`)
	}))
	defer srv.Close()

	cfg := Config{Clbug: ClbugConfig{Enabled: true, URL: srv.URL, MaxCandidates: 10}}
	d := New(cfg, fetcher.New(10*time.Second), "")
	got, err := d.clbugChannel(context.Background())
	if err != nil {
		t.Fatalf("clbugChannel 报错: %v", err)
	}
	if len(got) != 2 || got[0] != "http://a.com/sub.json" || got[1] != "http://b.com/x.json" {
		t.Errorf("data-url 提取错误: %v", got)
	}
}

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discovered.yaml")
	sources := []model.SourceConfig{
		{Name: "发现·a.com", URL: "http://a.com/motif/api.php/provide/vod", Type: "collect"},
	}
	if err := Save(path, sources); err != nil {
		t.Fatalf("Save 报错: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "发现·a.com") {
		t.Error("保存内容缺少源信息")
	}
	// 与 main.go loadDiscovered 同格式可解析
	var doc struct {
		Sources []model.SourceConfig `yaml:"sources"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(doc.Sources) != 1 || doc.Sources[0].URL != sources[0].URL {
		t.Errorf("解析结果错误: %+v", doc.Sources)
	}
}
