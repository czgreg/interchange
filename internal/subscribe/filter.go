package subscribe

import "strings"

// filterOutbounds drops nodes that should never reach the mihomo config:
// info-banner pseudo-nodes that many providers ship as fake outbounds
// (remaining quota, expiry, homepage URL). They'd land in the urltest
// pool and probe forever. Match by tag substring against provider conventions.
func filterOutbounds(in []Outbound) []Outbound {
	out := make([]Outbound, 0, len(in))
	for _, o := range in {
		if isBannerTag(o.Tag()) {
			continue
		}
		out = append(out, o)
	}
	return out
}

// bannerSubstrings catches Chinese subscription banner conventions. Match is
// substring + case-insensitive on ASCII; CJK characters compare literally.
var bannerSubstrings = []string{
	"剩余流量", "套餐到期", "距离下次", "过期时间", "已用流量", "总流量",
	"官网", "订阅", "网址", "重置", "续费", "购买", "充值",
	"工单", "客服", "公告", "通知", "导航", "收藏",
	"重新导入", "请尝试", "节点超时", "套餐",
	"telegram", "tg频道", "tg群",
}

func isBannerTag(tag string) bool {
	if tag == "" {
		return false
	}
	// Real proxy nodes don't embed URLs in their tag — providers do this to
	// advertise their site. Use as a structural fallback for tags that don't
	// match any keyword above.
	if strings.Contains(tag, "://") {
		return true
	}
	low := strings.ToLower(tag)
	for _, s := range bannerSubstrings {
		if strings.Contains(low, strings.ToLower(s)) {
			return true
		}
	}
	return false
}
