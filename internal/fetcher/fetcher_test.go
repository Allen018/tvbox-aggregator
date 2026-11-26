package fetcher

import (
	"testing"

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
