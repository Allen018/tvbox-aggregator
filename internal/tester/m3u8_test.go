package tester

import (
	"strings"
	"testing"
)

const masterPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=1500000,RESOLUTION=1280x720
720p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
1080p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=854x480
480p/index.m3u8
`

func TestIsMaster(t *testing.T) {
	if !IsMaster(masterPlaylist) {
		t.Error("主播放列表应被识别")
	}
	if IsMaster("#EXTM3U\n#EXTINF:10,\nseg0.ts\n") {
		t.Error("媒体播放列表不应被识别为主列表")
	}
}

func TestParseMasterAndResolveBest(t *testing.T) {
	subs, err := ParseMaster(masterPlaylist, "https://cdn.example.com/master.m3u8")
	if err != nil {
		t.Fatalf("ParseMaster 报错: %v", err)
	}
	if len(subs) != 3 {
		t.Fatalf("期望 3 条轨道，实际 %d", len(subs))
	}
	// 相对 URI 应解析为绝对地址
	if subs[0].URL != "https://cdn.example.com/720p/index.m3u8" {
		t.Errorf("相对 URI 解析失败: %q", subs[0].URL)
	}
	if subs[0].Bandwidth != 1500000 || subs[0].Resolution != "1280x720" {
		t.Errorf("BANDWIDTH/RESOLUTION 解析失败: %+v", subs[0])
	}

	best := ResolveBest(subs)
	if best.Bandwidth != 5000000 || best.URL != "https://cdn.example.com/1080p/index.m3u8" {
		t.Errorf("应选择最高码率轨道，实际 %+v", best)
	}
}

func TestParseMedia(t *testing.T) {
	content := `#EXTM3U
#EXT-X-VERSION:3
#EXTINF:10.0,
seg0.ts
#EXT-X-DISCONTINUITY
#EXTINF:8.5,
https://other.example.com/ad/seg1.ts
#EXTINF:10.0,
seg2.ts
#EXT-X-ENDLIST
`
	pl, err := ParseMedia(content, "https://cdn.example.com/a/index.m3u8")
	if err != nil {
		t.Fatalf("ParseMedia 报错: %v", err)
	}
	if len(pl.Segments) != 3 {
		t.Fatalf("期望 3 个分片，实际 %d", len(pl.Segments))
	}
	if pl.Discontinuities != 1 {
		t.Errorf("期望 1 个断点，实际 %d", pl.Discontinuities)
	}
	// 相对/绝对 URI 均正确解析
	if pl.Segments[0].URL != "https://cdn.example.com/a/seg0.ts" {
		t.Errorf("相对 URI 解析失败: %q", pl.Segments[0].URL)
	}
	if pl.Segments[1].URL != "https://other.example.com/ad/seg1.ts" {
		t.Errorf("绝对 URI 解析失败: %q", pl.Segments[1].URL)
	}
	if pl.Segments[0].Duration != 10.0 || pl.Segments[1].Duration != 8.5 {
		t.Errorf("EXTINF 时长解析失败: %+v", pl.Segments)
	}
}

func TestParseMediaEmpty(t *testing.T) {
	if _, err := ParseMedia("#EXTM3U\n#EXT-X-ENDLIST\n", "https://a.com/i.m3u8"); err == nil {
		t.Error("无分片的播放列表应报错")
	}
}

func TestDetectAdClean(t *testing.T) {
	// 10 个同域分片、无断点：不应判定为含广告
	clean := "#EXTM3U\n"
	for i := 0; i < 10; i++ {
		clean += "#EXTINF:10.0,\nhttps://cdn.example.com/v/seg" + strings.TrimSpace(string(rune('0'+i))) + ".ts\n"
	}
	pl, err := ParseMedia(clean, "https://cdn.example.com/i.m3u8")
	if err != nil {
		t.Fatalf("ParseMedia 报错: %v", err)
	}
	rep := DetectAd(pl)
	if rep.Suspicious() {
		t.Errorf("干净播放列表不应判定含广告: %+v", rep)
	}
}

func TestDetectAdKeyword(t *testing.T) {
	// 分片路径含 /ad/ 特征段：应判定含广告
	content := "#EXTM3U\n#EXTINF:5.0,\nhttps://cdn.example.com/ad/seg0.ts\n#EXTINF:10.0,\nhttps://cdn.example.com/v/seg1.ts\n"
	pl, err := ParseMedia(content, "https://cdn.example.com/i.m3u8")
	if err != nil {
		t.Fatalf("ParseMedia 报错: %v", err)
	}
	rep := DetectAd(pl)
	if rep.KeywordHits != 1 || !rep.Suspicious() {
		t.Errorf("应命中广告特征词: %+v", rep)
	}
}

func TestDetectAdDomainChangeAndDiscontinuity(t *testing.T) {
	// 频繁断点 + 域名突变：应判定含广告
	content := "#EXTM3U\n" +
		"#EXTINF:10.0,\nhttps://cdn.example.com/v/seg0.ts\n" +
		"#EXT-X-DISCONTINUITY\n" +
		"#EXTINF:10.0,\nhttps://ads.example.net/v/seg1.ts\n" +
		"#EXT-X-DISCONTINUITY\n" +
		"#EXTINF:10.0,\nhttps://cdn.example.com/v/seg2.ts\n" +
		"#EXT-X-DISCONTINUITY\n" +
		"#EXTINF:10.0,\nhttps://ads.example.net/v/seg3.ts\n"
	pl, err := ParseMedia(content, "https://cdn.example.com/i.m3u8")
	if err != nil {
		t.Fatalf("ParseMedia 报错: %v", err)
	}
	rep := DetectAd(pl)
	if rep.Discontinuities != 3 {
		t.Errorf("断点数=%d，期望 3", rep.Discontinuities)
	}
	if rep.DomainChanges != 3 {
		t.Errorf("域名突变=%d，期望 3", rep.DomainChanges)
	}
	if !rep.Suspicious() {
		t.Errorf("频繁断点+域名突变应判定含广告: %+v", rep)
	}
}

func TestDetectAdNotSuspicious(t *testing.T) {
	var rep AdReport
	if rep.Suspicious() {
		t.Error("空报告不应判定含广告")
	}
}
