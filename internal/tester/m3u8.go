// Package tester 实现 m3u8 递归解析与 Range 分片测速。
package tester

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Segment 媒体播放列表中的单个分片。
type Segment struct {
	URL      string
	Duration float64
}

// SubStream 主播放列表中的一条码率轨道。
type SubStream struct {
	URL        string
	Bandwidth  int64
	Resolution string
}

// Playlist 解析后的播放列表。
type Playlist struct {
	Segments        []Segment
	SubStreams      []SubStream
	Discontinuities int
}

// IsMaster 判断 m3u8 内容是否为主播放列表（含 STREAM-INF 标签）。
func IsMaster(content string) bool {
	return strings.Contains(content, "#EXT-X-STREAM-INF")
}

// ParseMaster 解析主播放列表，返回全部码率轨道（纯解析，不发起网络请求）。
func ParseMaster(content, baseURL string) ([]SubStream, error) {
	lines := splitLines(content)
	var subs []SubStream
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			continue
		}
		bandwidth, resolution := parseStreamInf(line)
		uri := nextURILine(lines, i+1)
		if uri == "" {
			continue
		}
		subs = append(subs, SubStream{
			URL:        resolveURL(baseURL, uri),
			Bandwidth:  bandwidth,
			Resolution: resolution,
		})
	}
	if len(subs) == 0 {
		return nil, fmt.Errorf("主播放列表无有效子轨道")
	}
	return subs, nil
}

// ResolveBest 选择最高码率轨道。
func ResolveBest(subs []SubStream) SubStream {
	best := subs[0]
	for _, s := range subs[1:] {
		if s.Bandwidth > best.Bandwidth {
			best = s
		}
	}
	return best
}

// parseStreamInf 解析 #EXT-X-STREAM-INF 标签中的码率与分辨率。
func parseStreamInf(line string) (int64, string) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return 0, ""
	}
	attrs := line[idx+1:]
	var bandwidth int64
	var resolution string
	for _, part := range strings.Split(attrs, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "BANDWIDTH":
			bandwidth, _ = strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
		case "RESOLUTION":
			resolution = strings.TrimSpace(kv[1])
		}
	}
	return bandwidth, resolution
}

// ParseMedia 解析媒体播放列表（分片列表），相对 URI 基于 baseURL 解析。
func ParseMedia(content, baseURL string) (*Playlist, error) {
	pl := &Playlist{}
	lines := splitLines(content)
	var pendingDuration float64
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXTINF"):
			pendingDuration = parseExtInfDuration(line)
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY"):
			pl.Discontinuities++
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		default:
			pl.Segments = append(pl.Segments, Segment{
				URL:      resolveURL(baseURL, line),
				Duration: pendingDuration,
			})
			pendingDuration = 0
		}
	}
	if len(pl.Segments) == 0 {
		return nil, fmt.Errorf("媒体播放列表无有效分片")
	}
	return pl, nil
}

// parseExtInfDuration 解析 "#EXTINF:10.0," 中的时长（秒）。
func parseExtInfDuration(line string) float64 {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return 0
	}
	rest := line[idx+1:]
	if j := strings.Index(rest, ","); j >= 0 {
		rest = rest[:j]
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	return d
}

// nextURILine 返回从 lines[from] 开始的第一个非注释、非空行（子列表/分片 URI）。
func nextURILine(lines []string, from int) string {
	for i := from; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

func splitLines(content string) []string {
	return strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
}

// resolveURL 将相对 URI 解析为绝对地址。
func resolveURL(base, uri string) string {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return ""
	}
	if strings.Contains(uri, "://") {
		return uri
	}
	u, err := url.Parse(base)
	if err != nil {
		return uri
	}
	ref, err := u.Parse(uri)
	if err != nil {
		return uri
	}
	return ref.String()
}

// ---------------------------------------------------------------------------
// 切片广告特征检测
// ---------------------------------------------------------------------------

// AdReport 切片广告特征检测结果。
type AdReport struct {
	Discontinuities    int `json:"discontinuities"`
	DomainChanges      int `json:"domain_changes"`
	KeywordHits        int `json:"keyword_hits"`
	SuspiciousSegments int `json:"suspicious_segments"`
	TotalSegments      int `json:"total_segments"`
}

// adSegmentNames 分片路径中的广告特征片段（精确匹配路径段，避免 "ad" 子串误判）。
var adSegmentNames = map[string]bool{
	"ad":           true,
	"ads":          true,
	"ad.ts":        true,
	"ad.mp4":       true,
	"广告":          true,
	"daohang":      true,
	"promo":        true,
	"sponsor":      true,
	"interstitial": true,
}

// DetectAd 基于播放列表特征检测切片广告：
//  1. #EXT-X-DISCONTINUITY 标签数量（异常插播会频繁打断分片序列）；
//  2. 分片域名突变（广告通常托管于不同 CDN）；
//  3. 分片路径中的广告特征词。
//
// 返回评分式报告，由上层决定降权力度，不直接剔除源。
func DetectAd(pl *Playlist) AdReport {
	rep := AdReport{
		TotalSegments:   len(pl.Segments),
		Discontinuities: pl.Discontinuities,
	}
	prevDomain := ""
	for _, seg := range pl.Segments {
		u, err := url.Parse(seg.URL)
		if err == nil {
			domain := strings.ToLower(u.Host)
			if prevDomain != "" && domain != prevDomain {
				rep.DomainChanges++
			}
			prevDomain = domain
			for _, part := range strings.Split(u.Path, "/") {
				if adSegmentNames[strings.ToLower(strings.TrimSpace(part))] {
					rep.KeywordHits++
					rep.SuspiciousSegments++
					break
				}
			}
		}
	}
	// 每次断点前后各估 1 个可疑分片
	rep.SuspiciousSegments += pl.Discontinuities * 2
	if rep.SuspiciousSegments > rep.TotalSegments {
		rep.SuspiciousSegments = rep.TotalSegments
	}
	return rep
}

// Suspicious 综合判断是否存在高比例异常插播：
// 断点 ≥ 2 次，或可疑分片占比超过 30%，或命中广告特征词。
func (r AdReport) Suspicious() bool {
	if r.TotalSegments == 0 {
		return false
	}
	ratio := float64(r.SuspiciousSegments) / float64(r.TotalSegments)
	return r.Discontinuities >= 2 || ratio > 0.3 || r.KeywordHits > 0
}
