// Command argusd is the reference consumer of the argus library.
//
// 启动时打印一次设备列表, 之后通过回调实时打印上线 / 离线 / 状态变更事件。
// 同时监听 OpenWrt 系统日志, 捕获 WiFi 关联 / 断开 / DHCP 分配等底层事件。
//
// 除 stdout 日志外, 还同步注入:
//   - argusmetrics.Counters 用于累计决策/事件计数 (SIGUSR1 打印快照到 stderr)
//   - log/slog 结构化日志 hook (level/msg/attrs)
//   - 可选的 argusweb 本地仪表板 (-listen=127.0.0.1:9099)
//
// 信号:
//   - SIGINT  / SIGTERM: 优雅退出
//   - SIGHUP:            停止并重启 Watcher (保留 known / cooldown / 已探测 Fetcher)
//   - SIGUSR1:           打印一次 argusmetrics 快照到 stderr (不影响正在运行的 Watcher)
//
// 标志:
//   - -listen=addr     启动内置 Web UI / SSE 仪表板 (如 127.0.0.1:9099)
//
// 环境变量:
//   - ARGUSD_DEBUG=1   启用 slog Debug 级别 + 展开决策 trace
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/xxl6097/argus-app/interval/web"
	owrt "github.com/xxl6097/argusd"
	"github.com/xxl6097/argusd/argusmetrics"
)

// Version info populated by -ldflags at release time. When building
// locally via `go build`, these default to "dev" / "" / "".
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	listen := flag.String("listen", "", "optional Web UI listen address (e.g. 127.0.0.1:9099); empty disables")
	aliasesPath := flag.String("aliases", "/etc/argusd/aliases.json", "path to the Web UI alias store (MAC -> friendly name); empty keeps aliases in-memory only")
	settingsPath := flag.String("settings", "/etc/argusd/settings.json", "path to the Web UI settings (me MAC + workday window); empty keeps settings in-memory only")
	overridesPath := flag.String("overrides", "/etc/argusd/overrides.json", "path to manual worktime overrides (per MAC per date); empty disables manual edits")
	notifyPath := flag.String("notifications", "/etc/argusd/notifications.json", "path to per-device webhook/ntfy configs; empty disables notifications tab")
	holidaysPath := flag.String("holidays", "/etc/argusd/holidays.json", "path to manual legal holiday / 调休 workday calendar; empty disables manual entries")
	holidaysSystemPath := flag.String("holidays-system", "/etc/argusd/holidays_system.json", "path to auto-fetched (timor.tech) holiday cache; empty disables auto-refresh")
	holidaysYearsAhead := flag.Int("holidays-years", 10, "how many years ahead to fetch on each refresh (current year + N-1 future). The State Council typically publishes only the current year, so unpublished years silently no-op.")
	historyDir := flag.String("history-dir", "/etc/argusd/history", "directory for per-MAC online/offline history; empty disables history + worktime")
	flag.Parse()

	if *showVersion {
		fmt.Printf("argus-app %s (commit %s, built %s)\n", Version, Commit, Date)
		return
	}

	log.SetFlags(log.LstdFlags)
	log.Printf("argus-app %s (commit %s, built %s)", Version, Commit, Date)
	owrt.SetupLocalTimezone()

	// structured logger: slog.TextHandler → stderr (人可读, 同时可被 systemd 采集)
	slogLevel := slog.LevelInfo
	if os.Getenv("ARGUSD_DEBUG") == "1" {
		slogLevel = slog.LevelDebug
	}
	slogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel}))

	metrics := argusmetrics.New()

	// SIGINT / SIGTERM 触发整体退出
	exitCtx, stopExit := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopExit()

	// SIGHUP 触发 Stop + Run 重启 (热重载模式, 保留 known/cooldown)
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	// SIGUSR1 触发指标快照打印 — 独立 goroutine, 不与 Run 生命周期耦合。
	// 放在主 for-select 里会导致每次 SIGUSR1 都重入 for 循环, 开启第二个 Run,
	// 第二个 Run 立刻拿到 ErrAlreadyRunning, 被 log.Fatalf 杀死进程。
	sigusr1 := make(chan os.Signal, 1)
	signal.Notify(sigusr1, syscall.SIGUSR1)
	defer signal.Stop(sigusr1)
	go func() {
		for {
			select {
			case <-exitCtx.Done():
				return
			case <-sigusr1:
				printMetricsSnapshot(metrics)
			}
		}
	}()

	w := owrt.New(
		owrt.OnFetcherDetected(func(k owrt.FetcherKind) {
			log.Printf("已选择数据源: %s", k)
		}),
		owrt.WithDecisionHandler(func(d owrt.Decision) {
			metrics.OnDecision(d)
			if os.Getenv("ARGUSD_DEBUG") == "1" {
				onDecision(d)
			}
		}),
		owrt.WithLogger(func(ctx context.Context, level owrt.LogLevel, msg string, attrs ...owrt.LogAttr) {
			sa := make([]slog.Attr, 0, len(attrs))
			for _, a := range attrs {
				sa = append(sa, slog.Any(a.Key, a.Value))
			}
			slogger.LogAttrs(ctx, slog.Level(level), msg, sa...)
		}),
	)

	// Optional built-in Web UI. Launched on a goroutine bound to exitCtx;
	// graceful shutdown on SIGINT/SIGTERM via httpSrv.Shutdown.
	var webServer *web.Server
	var httpSrv *http.Server
	if *listen != "" {
		aliasStore := web.NewAliasStore(*aliasesPath)
		opts := []web.Option{
			web.WithAliases(aliasStore),
			web.WithSettings(web.NewSettingsStore(*settingsPath)),
		}
		if *overridesPath != "" {
			opts = append(opts, web.WithOverrides(web.NewOverrideStore(*overridesPath, aliasStore)))
			log.Printf("手动工时编辑已启用 (file=%s, key=alias)", *overridesPath)
		}
		if *notifyPath != "" {
			notifyStore := web.NewNotifyStore(*notifyPath, aliasStore)
			notifier := web.NewNotifier(notifyStore, nil)
			opts = append(opts, web.WithNotifications(notifyStore, notifier))
			log.Printf("通知设置已启用 (file=%s, webhook+ntfy)", *notifyPath)
		}
		if *holidaysPath != "" {
			hs := web.NewHolidayStoreWithSystem(*holidaysPath, *holidaysSystemPath)
			// Offline / first-boot safety net: if the manual layer is
			// empty AND we can't reach the network yet, at least seed
			// the current year from the built-in 2026 table. Harmless
			// when online — the API layer shadows these anyway.
			hs.SeedDefaultsIfEmpty(web.CN2026Holidays())
			opts = append(opts, web.WithHolidays(hs))
			if *holidaysSystemPath != "" {
				hs.StartAutoRefresh(*holidaysYearsAhead)
				log.Printf("法定节假日已启用 (manual=%s, system=%s, years=%d, refresh=03:00 daily)",
					*holidaysPath, *holidaysSystemPath, *holidaysYearsAhead)
			} else {
				log.Printf("法定节假日已启用 (manual=%s, auto-refresh disabled)", *holidaysPath)
			}
		}
		if *historyDir != "" {
			opts = append(opts, web.WithHistory(web.NewHistoryStore(*historyDir)))
			log.Printf("设备上下线历史已启用 (dir=%s, retention=30d)", *historyDir)
		}
		// Auto-detect OpenWrt uci; enable /api/dhcp + dashboard static-IP
		// button when available, silently skip otherwise (dev laptops).
		if dhcp, err := web.NewUCIDHCPManager(); err == nil {
			opts = append(opts, web.WithDHCPManager(dhcp))
			log.Println("DHCP 静态租约管理已启用 (uci)")
		} else {
			log.Printf("DHCP 静态租约管理未启用: %v", err)
		}
		webServer = web.NewServer(w, opts...)
		httpSrv = &http.Server{
			Addr:         *listen,
			Handler:      webServer,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 0, // SSE streams are long-lived; 0 = no write timeout
			IdleTimeout:  120 * time.Second,
		}
		go func() {
			log.Printf("Web UI 监听 %s (GET / 面板, /api/devices 快照, /api/events SSE)", *listen)
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Web UI 启动失败: %v", err)
			}
		}()
	}

	devices, err := w.List(exitCtx)
	if err != nil {
		log.Fatalf("初始拉取失败: %v", err)
	}
	fmt.Println(owrt.RenderTable(devices))

	// Seed the history store with the currently-online baseline so
	// devices that never flap during this session still show as
	// "online since boot" on the timeline.
	if webServer != nil && webServer.History() != nil {
		now := time.Now()
		for _, d := range devices {
			webServer.History().SeedBaseline(d, now)
		}
	}
	// Alias-keyed notify entries need the watcher's known set to
	// resolve to MACs; trigger subscriptions now that it's populated.
	if webServer != nil {
		webServer.RefreshNotifySubs()
	}

	// 启动外部系统日志监听用于展示 (库内部的 syslog 已自动纳入离线判断)
	// 同时把每条系统日志事件喂给 web.Server 作为来源标注的 hint, 这样
	// /api/history 落盘的每个 ONLINE/OFFLINE 都能携带 src 字段
	// (syslog:WPA_COMPLETE / fetcher:ahsapd / ...)
	go func() {
		handler := func(e owrt.SyslogEvent) {
			onSyslog(e)
			if webServer != nil {
				webServer.OnSyslog(e)
			}
		}
		if err := owrt.WatchSyslog(exitCtx, handler, onError); err != nil {
			log.Printf("系统日志监听异常退出: %v", err)
		}
	}()

	log.Println("开始监听设备状态变化, Ctrl+C 退出, SIGHUP 重启, SIGUSR1 打印指标 ...")
	generation := 1
	for {
		// 每一轮 Run 用一个派生 ctx, 可被 SIGHUP 单独取消而不影响 exitCtx
		runCtx, cancelRun := context.WithCancel(exitCtx)
		runDone := make(chan error, 1)
		go func() {
			runDone <- w.Run(runCtx, func(e owrt.Event) {
				metrics.OnEvent(e)
				if webServer != nil {
					webServer.OnEvent(e)
				}
				onEvent(e)
			}, onError)
		}()

		select {
		case err := <-runDone:
			cancelRun()
			shutdownWeb(httpSrv, webServer)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Fatalf("监听异常退出: %v", err)
			}
			printMetricsSnapshot(metrics)
			log.Println("收到退出信号, 程序结束")
			return

		case <-exitCtx.Done():
			// SIGINT / SIGTERM: 等当前 Run 退出再返回
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = w.Stop(stopCtx)
			stopCancel()
			cancelRun()
			<-runDone
			shutdownWeb(httpSrv, webServer)
			printMetricsSnapshot(metrics)
			log.Println("收到退出信号, 程序结束")
			return

		case <-sighup:
			// SIGHUP: 停 Watcher, 打印快照, 继续下一轮 Run (保留 known / cooldown)
			log.Printf("收到 SIGHUP, 重启 Watcher (第 %d 轮 → 第 %d 轮)", generation, generation+1)
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := w.Stop(stopCtx); err != nil {
				log.Printf("Stop 超时: %v", err)
			}
			stopCancel()
			cancelRun()
			<-runDone // 等 Run 真正返回
			snap := w.Known()
			log.Printf("[重启] 保留 %d 台已知设备, 冷却/抖动状态亦保留, 瞬态计数已重置", len(snap))
			generation++
			// 继续 for 循环, 开始下一轮 Run
		}
	}
}

// shutdownWeb drains SSE subscribers and gives the HTTP server 3 s to
// finish in-flight requests. No-op when the Web UI wasn't enabled.
func shutdownWeb(httpSrv *http.Server, web *web.Server) {
	if httpSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	if web != nil {
		web.Shutdown(ctx)
	}
}

// printMetricsSnapshot 打印 argusmetrics 当前快照到 stderr, 按 key 字母序。
func printMetricsSnapshot(m *argusmetrics.Counters) {
	snap := m.Snapshot()
	if len(snap) == 0 {
		fmt.Fprintln(os.Stderr, "[metrics] (no decisions yet)")
		return
	}
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintln(os.Stderr, "[metrics] --- snapshot ---")
	for _, k := range keys {
		fmt.Fprintf(os.Stderr, "[metrics]   %-32s = %d\n", k, snap[k])
	}
}

// onEvent 是库回调的事件处理函数, 演示如何按事件类型展示信息。
// 库返回的 e.Kind 是英文状态码 (ONLINE/OFFLINE/CHANGE), 通过 Label() 拿中文文案打印。
func onEvent(e owrt.Event) {
	ts := e.Time.Format("2006-01-02 15:04:05")
	switch e.Kind {
	case owrt.EventOnline, owrt.EventOffline:
		fmt.Printf("[%s] %s %s\n", ts, e.Kind.Label(), e.Device)
	case owrt.EventChange:
		parts := make([]string, 0, len(e.Changes))
		for _, c := range e.Changes {
			parts = append(parts, fmt.Sprintf("%s %q→%q", c.Field, c.Old, c.New))
		}
		fmt.Printf("[%s] %s %s (%s)\n", ts, e.Kind.Label(), strings.ToUpper(e.Device.MAC), strings.Join(parts, ", "))
	}
}

// onSyslog 处理系统日志事件。
func onSyslog(e owrt.SyslogEvent) {
	ts := e.Time.Format("2006-01-02 15:04:05")
	switch e.Kind {
	case owrt.SyslogDHCPAck:
		fmt.Printf("[%s] [系统日志] %s %s IP=%s\n", ts, e.Kind.Label(), strings.ToUpper(e.MAC), e.IP)
	default:
		iface := ""
		if e.Iface != "" {
			iface = " 接口=" + e.Iface
		}
		fmt.Printf("[%s] [系统日志] %s %s%s\n", ts, e.Kind.Label(), strings.ToUpper(e.MAC), iface)
	}
}

// onDecision 打印 Watcher 内部判定链路, 用于调试和观测。
// 业务消费者通常不需要关心, 本示例打印是为了演示决策过程可见性。
// 仅在 ARGUSD_DEBUG=1 时启用, 因为频率较高 (每秒几十行)。
func onDecision(d owrt.Decision) {
	ts := d.Time.Format("2006-01-02 15:04:05")
	mac := strings.ToUpper(d.MAC)
	if d.Detail == "" {
		fmt.Printf("[%s] [决策] %s %s\n", ts, d.Kind.Label(), mac)
	} else {
		fmt.Printf("[%s] [决策] %s %s (%s)\n", ts, d.Kind.Label(), mac, d.Detail)
	}
}

// onError 处理库上报的非致命拉取错误。
func onError(err error) {
	log.Printf("拉取设备列表失败: %v", err)
}
