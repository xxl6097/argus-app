// notify_dispatch.go — webhook/ntfy event routing for OnEvent.
//
// dispatchNotify takes one argus.Event and decides which channels
// fire. Two layers:
//
//   1. classifyPunchEvent — for devices flagged as 打卡设备, decides
//      whether this is a real check-in/check-out (heavyweight body
//      with worktime stats) or a transient flap (lightweight body,
//      per-device webhook suppressed).
//
//   2. opt-in gate — only devices that have a per-device notify
//      config (webhook_url or ntfy_*) fire any webhook at all.
//      Without that gate the global webhook would broadcast every
//      neighbour's phone wandering past the AP.
//
// The body is rendered by formatNotifyMarkdown and decorated with the
// dingtalk/feishu keyword via appendWebhookKeyword if one is set in
// settings.

package web

import (
	"fmt"
	"os"
	"strings"
	"time"

	argus "github.com/xxl6097/argusd"
	"github.com/xxl6097/argus-app/interval/store/override"
	"github.com/xxl6097/argus-app/interval/store/notify"
	"github.com/xxl6097/argus-app/interval/store/history"
	"github.com/xxl6097/argus-app/interval/util"
)

// chineseWeekdays maps Go's time.Weekday to Chinese day names.
var chineseWeekdays = []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

// historySyslogLabels maps the SyslogKind.String() values that the
// argusd library emits to the same Chinese pills the dashboard renders.
// Keep in sync with HISTORY_SYSLOG_LABELS in dashboard.html.
var historySyslogLabels = map[string]string{
	"WIFI_CONNECT":    "无线接入",
	"WPA_COMPLETE":    "认证完成",
	"DHCP_ACK":        "DHCP 分配",
	"MACTABLE_INSERT": "MAC 表新增",
	"WIFI_DISCONNECT": "无线断开",
	"DEAUTH":          "认证踢出",
	"MACTABLE_DELETE": "MAC 表移除",
}

// sourceLabel renders an attribution tag (as produced by Server.sourceFor)
// into a human-readable Chinese phrase suitable for webhook / ntfy bodies.
// Returns the empty string when src is empty so callers can soft-skip.
func sourceLabel(src string) string {
	if src == "" {
		return ""
	}
	if src == "seed" {
		return "启动快照"
	}
	if i := strings.Index(src, ":"); i > 0 {
		head, tail := src[:i], src[i+1:]
		switch head {
		case "syslog":
			if v, ok := historySyslogLabels[tail]; ok {
				return v
			}
			return tail
		case "fetcher":
			return tail + " 轮询"
		}
	}
	return src
}

// dispatchNotify formats a markdown payload and ships it to the
// global webhook (settings.GlobalWebhookURL) and the per-device
// webhook/ntfy destinations. Either / both may be enabled; the
// payload carries a `scope` field ("global" or "device") so the
// receiver can tell them apart.
//
// Punch-device transient suppression: for devices flagged as 打卡设备,
// only the "real" check-in (first ONLINE of the day) and check-out
// (OFFLINE at/after WorkEnd) fire the heavyweight per-device webhook
// with worktime stats. Lunch returns / WiFi blips during the day still
// fire the *global* webhook (so the activity log stays complete) but
// suppress the per-device channel and downgrade the body to the
// lightweight "上线啦/下线啦" form — otherwise users get spammed with
// "【alias】上班了" every time the phone reconnects.
func (s *Server) dispatchNotify(e argus.Event) {
	if s.notifier == nil {
		return
	}
	if e.Kind != argus.EventOnline && e.Kind != argus.EventOffline {
		return
	}
	mac := util.NormalizeMAC(e.Device.MAC)
	macU := strings.ToUpper(mac)
	alias := ""
	if s.aliases != nil {
		alias = s.aliases.Lookup(mac)
	}
	displayName := alias
	if displayName == "" {
		displayName = e.Device.Hostname
	}
	if displayName == "" {
		displayName = macU
	}
	isPunch := s.settings != nil && s.settings.IsPunch(mac)

	when := util.NonZeroTime(e.Time)
	// Reuse the same attribution that the history store uses, so the
	// webhook body matches the timeline pill exactly. sourceFor() peeks
	// at the syslog hint cache; we have to call it BEFORE OnEvent's
	// history.Record runs (otherwise the hint TTL window closes), but
	// since dispatchNotify itself runs from inside OnEvent right after
	// Record, the timestamps line up identically.
	source := s.sourceFor(e)
	sourceText := sourceLabel(source)

	// Classify: for punch devices, decide whether this event is a
	// "real" check-in/check-out (fires both global + per-device) or
	// transient noise (global only, lightweight body).
	cls := s.classifyPunchEvent(e, isPunch, when)

	// Per-device webhook gets the heavyweight worktime body only for
	// real check-in/check-out. Global webhook gets the same upgraded
	// body on those events; on transient events both fall back to the
	// lightweight form.
	deviceIsPunch := cls == punchEventCheckIn || cls == punchEventCheckOut
	globalIsPunch := deviceIsPunch

	// Resolve dingtalk/feishu keyword once — same suffix appended to
	// every body, regardless of which webhook channel it lands on.
	keyword := ""
	if s.settings != nil {
		keyword = s.settings.Get().WebhookKeyword
	}

	// "Opt-in" gate, split into two independent decisions:
	//
	//   - global webhook fires when the device is in settings.WebhookMACs
	//     (toggled from the per-device 上下线记录 tab). This used to piggy-
	//     back on per-device notify config, but the user wanted a separate
	//     switch — some devices want to be in the global activity log
	//     without setting up a dedicated webhook themselves.
	//
	//   - per-device webhook + ntfy fires when notify config exists,
	//     same as before.
	//
	// If neither gate opens, return early and skip both branches.
	hasNotifyConfig := false
	var deviceCfg notify.NotifyConfig
	if s.notifyStore != nil {
		if cfg, ok := s.notifyStore.Lookup(e.Device.MAC); ok {
			hasNotifyConfig = true
			deviceCfg = cfg
		}
	}
	wantGlobal := s.settings != nil && s.settings.IsWebhook(mac)
	if !wantGlobal && !hasNotifyConfig {
		return // device opted into neither channel → silent
	}

	// 1) Global webhook (settings-level): fires only when the device is
	//    in settings.WebhookMACs AND a global URL is configured.
	if wantGlobal && s.settings != nil {
		if gURL := s.settings.Get().GlobalWebhookURL; gURL != "" {
			gp := s.formatNotifyMarkdown(e, when, displayName, mac, globalIsPunch, sourceText)
			if source != "" {
				gp["source"] = source
			}
			if sourceText != "" {
				gp["source_label"] = sourceText
			}
			gp["scope"] = "global"
			appendWebhookKeyword(gp, keyword)
			s.notifier.Dispatch(mac, notify.NotifyConfig{WebhookURL: gURL}, gp, e.Kind.String())
		}
	}

	// 2) Per-device webhook + ntfy: skipped for transient punch events
	//    AND when no per-device config exists.
	if cls == punchEventTransient {
		return
	}
	if !hasNotifyConfig {
		return
	}
	dp := s.formatNotifyMarkdown(e, when, displayName, mac, deviceIsPunch, sourceText)
	if source != "" {
		dp["source"] = source
	}
	if sourceText != "" {
		dp["source_label"] = sourceText
	}
	dp["scope"] = "device"
	appendWebhookKeyword(dp, keyword)
	s.notifier.Dispatch(mac, deviceCfg, dp, e.Kind.String())
}

// appendWebhookKeyword stamps `keyword` onto the payload's markdown
// title + text so dingtalk/feishu robots configured with a keyword
// security policy let the message through. No-op when keyword is
// empty or the payload doesn't carry a markdown sub-object.
func appendWebhookKeyword(payload map[string]any, keyword string) {
	if keyword == "" {
		return
	}
	md, ok := payload["markdown"].(map[string]interface{})
	if !ok {
		return
	}
	if t, ok := md["title"].(string); ok {
		md["title"] = t + " " + keyword
	}
	if t, ok := md["text"].(string); ok {
		md["text"] = t + "\n\n——" + keyword
	}
}

// punchEventClass is the classifier output for dispatchNotify.
type punchEventClass int

const (
	// punchEventNotPunch — this device isn't a 打卡设备; don't apply
	// any of the punch-aware suppression. dispatchNotify treats it
	// as a regular device.
	punchEventNotPunch punchEventClass = iota
	// punchEventCheckIn — first ONLINE of the day for a punch device.
	// Fires both global + per-device with the heavyweight 上班了 body.
	punchEventCheckIn
	// punchEventCheckOut — OFFLINE at/after WorkEnd for a punch device.
	// Fires both global + per-device with the heavyweight 下班了 body.
	punchEventCheckOut
	// punchEventTransient — punch device, but this is a same-day
	// re-ONLINE or a pre-WorkEnd OFFLINE. Fires global ONLY, with the
	// lightweight 上线啦/下线啦 body. Per-device is suppressed.
	punchEventTransient
)

// recordPunchCheckout writes today's last OFFLINE-after-WorkEnd time
// into overrides.json as the day's "out" for a punch device. Called
// from OnEvent for every event; no-ops on:
//   - non-OFFLINE events
//   - non-punch devices
//   - missing overrides / settings stores
//   - OFFLINE before WorkEnd (we want to capture the *real* checkout,
//     not lunch breaks)
//
// Last-write-wins semantics: every after-end OFFLINE overwrites the
// previous "out". The existing "in" (manually entered or seeded by
// a prior write) is preserved. If no override row exists yet for
// today, a new one is created with only "out" set; the worktime
// compute path treats missing "in" as "fall back to history".
//
// Why persist instead of just trusting history.jsonl? Two reasons:
//   1. The /api/worktime month report uses overrides as authoritative
//      when they exist. Writing a real checkout here means the daily
//      report reflects the user's "I left at X" rather than the last
//      probe seeing the device.
//   2. History can be pruned (30-day retention); overrides are not.
//      For long-term reporting, the override row is the durable
//      record of "what time did this person leave today".
func (s *Server) recordPunchCheckout(e argus.Event) {
	if e.Kind != argus.EventOffline {
		return
	}
	if s.overrides == nil || s.settings == nil {
		return
	}
	mac := util.NormalizeMAC(e.Device.MAC)
	if mac == "" || !s.settings.IsPunch(mac) {
		return
	}
	when := util.NonZeroTime(e.Time).In(time.Local)
	cfg := s.settings.Get()
	endSec, ok := util.ParseClock(cfg.WorkEnd)
	if !ok {
		return
	}
	nowSec := when.Hour()*3600 + when.Minute()*60 + when.Second()
	if nowSec < endSec {
		return // before WorkEnd — lunch / transient drop, not a real checkout
	}
	dateKey := when.Format("2006-01-02")
	outHHMM := when.Format("15:04")
	// Preserve any existing "in" so we don't clobber a manual arrival
	// entry the user filed earlier in the day.
	o, _ := s.overrides.Lookup(mac, dateKey)
	o.Out = outHHMM
	if err := s.overrides.Set(mac, dateKey, o); err != nil {
		// Non-fatal: best-effort here, don't break the event flow.
		fmt.Fprintf(os.Stderr, "recordPunchCheckout %s %s: %v\n", mac, dateKey, err)
	}
}

// classifyPunchEvent decides how to route e for a punch device. See
// punchEventClass docs for the four buckets. Returns punchEventNotPunch
// (i.e. "no special handling") whenever isPunch is false, history is
// unattached, or settings is unattached — so the suppression logic
// silently degrades to the legacy behaviour on test setups that don't
// wire up all stores.
func (s *Server) classifyPunchEvent(e argus.Event, isPunch bool, when time.Time) punchEventClass {
	if !isPunch || s.history == nil || s.settings == nil {
		return punchEventNotPunch
	}
	mac := util.NormalizeMAC(e.Device.MAC)
	whenLocal := when.In(time.Local)
	dayStart := time.Date(whenLocal.Year(), whenLocal.Month(), whenLocal.Day(), 0, 0, 0, 0, time.Local)
	// Slight forward padding: history.Record for THIS event has already
	// run by the time dispatchNotify gets here (see OnEvent ordering),
	// so we explicitly slice [00:00, when - 1ms] to count only PRIOR
	// transitions today.
	priorTo := whenLocal.Add(-time.Millisecond)
	entries, err := s.history.Query(mac, dayStart, priorTo)
	if err != nil {
		return punchEventCheckIn // fail-open: treat as real, never silently drop
	}
	switch e.Kind {
	case argus.EventOnline:
		// First ONLINE today = check-in. Any prior ONLINE today = transient.
		for _, h := range entries {
			if h.Kind == "ONLINE" {
				return punchEventTransient
			}
		}
		return punchEventCheckIn
	case argus.EventOffline:
		cfg := s.settings.Get()
		endSec, ok := util.ParseClock(cfg.WorkEnd)
		if !ok {
			return punchEventCheckOut // unparseable WorkEnd → never suppress
		}
		nowSec := whenLocal.Hour()*3600 + whenLocal.Minute()*60 + whenLocal.Second()
		if nowSec < endSec {
			return punchEventTransient
		}
		return punchEventCheckOut
	}
	return punchEventNotPunch
}

// formatNotifyMarkdown renders the per-device markdown body. Punch
// devices on ONLINE get worktime stats (today's overtime + month
// total); everything else gets the lightweight "上线啦/下线啦" form.
// sourceText is the Chinese attribution (sourceLabel(...)); empty
// string skips the line.
func (s *Server) formatNotifyMarkdown(e argus.Event, when time.Time, displayName, mac string, isPunch bool, sourceText string) map[string]any {
	when = when.In(time.Local)
	dateStr := when.Format("2006-01-02")
	weekday := chineseWeekdays[int(when.Weekday())]
	clock := when.Format("15:04:05")
	clockMs := fmt.Sprintf("%s.%03d", clock, when.Nanosecond()/1e6)
	host, _ := os.Hostname()
	if host == "" {
		host = "—"
	}
	ip := e.Device.IP
	if ip == "" {
		ip = "—"
	}

	markdown := make(map[string]interface{})
	var verb string
	var b strings.Builder
	if isPunch {
		if e.Kind == argus.EventOnline {
			verb = "上班了"
		} else if e.Kind == argus.EventOffline {
			verb = "下班了"
		} else {
			verb = e.Kind.String()
		}
		fmt.Fprintf(&b, "【%s】%s\n", displayName, verb)
		fmt.Fprintf(&b, "- 今天是 %s %s\n", dateStr, weekday)
		fmt.Fprintf(&b, "- 设备：%s\n", host)
		fmt.Fprintf(&b, "- 信号：%d\n", e.Device.RSSI)
		fmt.Fprintf(&b, "- Wi-Fi：%s\n", e.Device.SSID)
		fmt.Fprintf(&b, "- 频道：%s\n", e.Device.Radio)
		fmt.Fprintf(&b, "- 类别：%s\n", e.Device.Type)
		fmt.Fprintf(&b, "- IP地址：%s\n", ip)
		fmt.Fprintf(&b, "- Mac地址：%s\n", strings.ToLower(mac))
		// Worktime context — only meaningful when history+settings are
		// enabled. Soft-skip individual lines that can't be computed.
		if s.history != nil && s.settings != nil {
			cfg := s.settings.Get()
			day, _ := time.ParseInLocation("2006-01-02", dateStr, time.Local)
			from := day.Add(-24 * time.Hour)
			to := day.Add(48 * time.Hour)
			entries, _ := s.history.Query(mac, from, to)
			var override override.Override
			if s.overrides != nil {
				if o, ok := s.overrides.Lookup(mac, dateStr); ok {
					override = o
				}
			}
			rep := history.ComputeWorktime(mac, day, cfg.WorkStart, cfg.WorkEnd, entries, when, override, s.dayKindFor(day))
			// On ONLINE we expect FirstSeen to equal `when` (or be very
			// close); on OFFLINE we want LastSeen.
			if e.Kind == argus.EventOnline && rep.FirstSeenMs > 0 {
				fmt.Fprintf(&b, "- 上班时间：%s\n", time.UnixMilli(rep.FirstSeenMs).In(time.Local).Format("15:04:05"))
			} else if e.Kind == argus.EventOffline && rep.LastSeenMs > 0 {
				fmt.Fprintf(&b, "- 下班时间：%s\n", time.UnixMilli(rep.LastSeenMs).In(time.Local).Format("15:04:05"))
			}
			fmt.Fprintf(&b, "- 今日加班时长：%s\n", humanDuration(rep.OvertimeSecs))
			// Month total — current calendar month up to today (or
			// through the event day, whichever the event date implies).
			if monthOT, ok := s.monthOvertimeSecs(mac, day, when); ok {
				fmt.Fprintf(&b, "- 本月加班时长：%s\n", humanDuration(monthOT))
			}
		}
		if sourceText != "" {
			fmt.Fprintf(&b, "- 触发原因：%s\n", sourceText)
		}
		fmt.Fprintf(&b, "- 消息时间：%s", clockMs)
	} else {
		if e.Kind == argus.EventOnline {
			verb = "上线啦"
		} else if e.Kind == argus.EventOffline {
			verb = "下线啦"
		} else {
			verb = e.Kind.String()
		}
		fmt.Fprintf(&b, "【%s】%s\n", displayName, verb)
		fmt.Fprintf(&b, "- 今天是 %s %s\n", dateStr, weekday)
		fmt.Fprintf(&b, "- 设备：%s\n", host)
		fmt.Fprintf(&b, "- 信号：%d\n", e.Device.RSSI)
		fmt.Fprintf(&b, "- Wi-Fi：%s\n", e.Device.SSID)
		fmt.Fprintf(&b, "- 频道：%s\n", e.Device.Radio)
		fmt.Fprintf(&b, "- 类别：%s\n", e.Device.Type)
		fmt.Fprintf(&b, "- IP地址：%s\n", ip)
		fmt.Fprintf(&b, "- Mac地址：%s\n", strings.ToLower(mac))
		if sourceText != "" {
			fmt.Fprintf(&b, "- 触发原因：%s\n", sourceText)
		}
		fmt.Fprintf(&b, "- 消息时间：%s", clockMs)
	}
	payload := map[string]any{}
	payload["msgtype"] = "markdown"
	markdown["title"] = fmt.Sprintf("【%s】%s", displayName, verb)
	markdown["text"] = b.String()
	payload["markdown"] = markdown
	return payload
}

// monthOvertimeSecs sums overtime for the calendar month containing
// `day`, up to and including `now`. Returns false on missing stores.
func (s *Server) monthOvertimeSecs(mac string, day, now time.Time) (int64, bool) {
	if s.history == nil || s.settings == nil {
		return 0, false
	}
	cfg := s.settings.Get()
	monthStart := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location())
	monthEnd := monthStart.AddDate(0, 1, 0)
	from := monthStart.Add(-24 * time.Hour)
	to := monthEnd.Add(24 * time.Hour)
	entries, err := s.history.Query(mac, from, to)
	if err != nil {
		return 0, false
	}
	cap := monthEnd
	if now.Before(cap) {
		cap = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(24 * time.Hour)
	}
	var total int64
	for d := monthStart; d.Before(cap); d = d.AddDate(0, 0, 1) {
		var override override.Override
		if s.overrides != nil {
			if o, ok := s.overrides.Lookup(mac, d.Format("2006-01-02")); ok {
				override = o
			}
		}
		rep := history.ComputeWorktime(mac, d, cfg.WorkStart, cfg.WorkEnd, entries, now, override, s.dayKindFor(d))
		total += rep.OvertimeSecs
	}
	return total, true
}

// humanDuration renders seconds as "1h7m13s" / "45s" / "0s". Compact
// form to match the markdown spec (different from the dashboard's
// fully-spelled "1时7分13秒").
func humanDuration(secs int64) string {
	if secs <= 0 {
		return "0s"
	}
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}
