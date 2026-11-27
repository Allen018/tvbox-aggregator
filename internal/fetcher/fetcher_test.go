package fetcher

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"unicode/utf8"

	"tvbox-aggregator/internal/model"
)

func TestExtractPlayURLs(t *testing.T) {
	vl := &model.VodList{
		List: []model.VodItem{
			{
				PlayURL: "第1集$https://a.com/1.m3u8#第2集$https://a.com/2.m3u8#预告片$https://a.com/t.m3u8",
			},
			{
				// $$$ 分隔多来源；非 m3u8 地址应被过滤
				PlayURL: "第1集$https://b.com/1.m3u8$$$第1集$https://c.com/1.m3u8#第1集$https://d.com/1.mp4",
			},
		},
	}
	urls := ExtractPlayURLs(vl, 5)

	want := []string{
		"https://a.com/1.m3u8",
		"https://a.com/2.m3u8",
		"https://b.com/1.m3u8",
		"https://c.com/1.m3u8",
	}
	if len(urls) != len(want) {
		t.Fatalf("期望 %d 个地址，实际 %d: %v", len(want), len(urls), urls)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Errorf("第 %d 个地址=%q，期望 %q", i, urls[i], want[i])
		}
	}
}

func TestExtractPlayURLsMaxLimitAndDedup(t *testing.T) {
	vl := &model.VodList{
		List: []model.VodItem{
			{PlayURL: "第1集$https://a.com/1.m3u8#第2集$https://a.com/1.m3u8#第3集$https://a.com/3.m3u8"},
		},
	}
	urls := ExtractPlayURLs(vl, 2)
	if len(urls) != 2 {
		t.Fatalf("期望去重后上限 2 个，实际 %d: %v", len(urls), urls)
	}
	if urls[0] != "https://a.com/1.m3u8" || urls[1] != "https://a.com/3.m3u8" {
		t.Errorf("去重顺序错误: %v", urls)
	}
}

func TestExtractPlayURLsNoPlayURL(t *testing.T) {
	vl := &model.VodList{List: []model.VodItem{{PlayURL: "第1集$https://a.com/1.mp4"}}}
	if urls := ExtractPlayURLs(vl, 3); len(urls) != 0 {
		t.Errorf("无 m3u8 时应为空，实际 %v", urls)
	}
	if urls := ExtractPlayURLs(nil, 3); urls != nil {
		t.Error("nil 列表应返回 nil")
	}
}

func TestExtractPlayURLsSkipTrailer(t *testing.T) {
	vl := &model.VodList{
		List: []model.VodItem{
			{PlayURL: "正片$https://a.com/1.m3u8#预告$https://a.com/t.m3u8#片花$https://a.com/f.m3u8"},
		},
	}
	urls := ExtractPlayURLs(vl, 5)
	if len(urls) != 1 || urls[0] != "https://a.com/1.m3u8" {
		t.Errorf("预告片应被跳过，实际 %v", urls)
	}
}

func TestFetchSubscriptionSites(t *testing.T) {
	// 模拟订阅配置：含全局 spider、type 3 爬虫源、type 0 采集站
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"spider": "./fty.jar;md5;abc",
			"sites": [
				{"key": "k1", "name": "爬虫站点", "type": 3, "api": "csp_DouDouGuard"},
				{"key": "k2", "name": "采集站点", "type": 0, "api": "http://a.com/api.php/provide/vod"}
			]
		}`)
	}))
	defer srv.Close()

	f := New(10 * time.Second)
	res, err := f.FetchSubscription(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("FetchSubscription 报错: %v", err)
	}
	if len(res.Sites) != 2 {
		t.Fatalf("期望 2 个站点，实际 %d", len(res.Sites))
	}
	// type 3 应保留原始类型（客户端需要原始 type，不再映射为 1）
	if res.Sites[0].Type == nil || res.Sites[0].Type.Value != 3 {
		t.Errorf("type 3 应保留为 3，实际 %+v", res.Sites[0].Type)
	}
	// 上游 key 必须保留，站点级 jar 使用客户端读取的字段
	if res.Sites[0].Key != "k1" {
		t.Errorf("上游 key 丢失，实际 %q", res.Sites[0].Key)
	}
	// 全局 spider 继承为站点 jar，相对路径解析为绝对地址并保留 md5 后缀
	if res.Sites[0].Jar != srv.URL+"/fty.jar;md5;abc" {
		t.Errorf("jar 相对路径解析错误: %q", res.Sites[0].Jar)
	}
	// 采集站保持 type 0
	if res.Sites[1].Type == nil || res.Sites[1].Type.Value != 0 {
		t.Errorf("采集站 type 应为 0，实际 %+v", res.Sites[1].Type)
	}
	if res.Sites[1].API != "http://a.com/api.php/provide/vod" {
		t.Errorf("采集站 api 解析错误: %q", res.Sites[1].API)
	}
}

func TestFetchSubscriptionURLs(t *testing.T) {
	// 模拟地址列表格式
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"urls":[{"name":"子订阅一","url":"http://a.com/sub1.json"},{"name":"子订阅二","url":"http://b.com/sub2.json"}]}`)
	}))
	defer srv.Close()

	f := New(10 * time.Second)
	res, err := f.FetchSubscription(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("FetchSubscription 报错: %v", err)
	}
	if len(res.URLs) != 2 || res.URLs[0] != "http://a.com/sub1.json" || res.URLs[1] != "http://b.com/sub2.json" {
		t.Errorf("子订阅地址提取错误: %v", res.URLs)
	}
}

func TestFetchSubscriptionInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"foo":"bar"}`)
	}))
	defer srv.Close()

	f := New(10 * time.Second)
	if _, err := f.FetchSubscription(context.Background(), srv.URL, nil); err == nil {
		t.Error("无法识别的格式应报错")
	}
}

func TestTranscodeBodyGBK(t *testing.T) {
	// GBK 编码的中文 JSON："测试" 的 GBK 字节为 B2 E2 CA D4
	gbkJSON := []byte{
		0x7B, 0x22, 0x6E, 0x61, 0x6D, 0x65, 0x22, 0x3A, 0x22,
		0xB2, 0xE2, 0xCA, 0xD4,
		0x22, 0x7D,
	}
	if utf8.Valid(gbkJSON) {
		t.Fatal("测试数据应为非 UTF-8")
	}
	got := transcodeBody(gbkJSON)
	if !utf8.Valid(got) {
		t.Fatal("转码后应为合法 UTF-8")
	}
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("转码后解析失败: %v", err)
	}
	if m["name"] != "测试" {
		t.Errorf("转码内容错误: %q", m["name"])
	}
}

func TestTranscodeBodyUTF8Passthrough(t *testing.T) {
	utf8Body := []byte(`{"name":"测试"}`)
	if !utf8.Valid(utf8Body) {
		t.Fatal("测试数据应为合法 UTF-8")
	}
	got := transcodeBody(utf8Body)
	if string(got) != string(utf8Body) {
		t.Error("UTF-8 内容应原样透传")
	}
}

func TestStripJSONComments(t *testing.T) {
	// 顶部带说明注释的 JSON（订阅配置常见格式）
	body := []byte("//以下部分来自网络收集，仅供学习交流使用\n" +
		`{
	  "sites": [{"name":"站点","type":0,"api":"http://a.com/api"}]
	}`)
	got := stripJSONComments(body)
	var cfg model.TVBoxConfig
	if err := json.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("去注释后解析失败: %v", err)
	}
	if len(cfg.Sites) != 1 || cfg.Sites[0].Name != "站点" {
		t.Errorf("去注释后解析结果错误: %+v", cfg.Sites)
	}
}

func TestStripJSONCommentsKeepsInString(t *testing.T) {
	// 字符串值内的 // 不应被去除
	body := []byte(`{"url":"http://a.com//path","name":"x"}`)
	got := stripJSONComments(body)
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if m["url"] != "http://a.com//path" {
		t.Errorf("字符串内的 // 被误删: %q", m["url"])
	}
}
