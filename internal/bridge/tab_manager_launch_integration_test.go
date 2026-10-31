package bridge

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	bridgeruntime "github.com/pinchtab/pinchtab/internal/bridge/runtime"
	"github.com/pinchtab/pinchtab/internal/config"
	"github.com/pinchtab/pinchtab/internal/testbrowser"
)

func newLaunchTabBrowser(t *testing.T, options ...chromedp.ExecAllocatorOption) context.Context {
	t.Helper()
	chromePath := testbrowser.Path(t)
	profile := testbrowser.ProfileDir(t)
	options = append(append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(profile),
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
	), options...)
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), options...)
	ctx, cancel := chromedp.NewContext(alloc)
	ctx, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	t.Cleanup(func() {
		cancelTimeout()
		cancel()
		cancelAlloc()
	})
	if err := chromedp.Run(ctx); err != nil {
		t.Fatalf("start browser: %v", err)
	}
	// A dead launch-page session must fail before the target-create timeout.
	ctx, cancelOperations := context.WithTimeout(ctx, tabCreateTimeout-time.Second)
	t.Cleanup(cancelOperations)
	return ctx
}

func TestLaunchTargetCreateCloseAndReuse(t *testing.T) {
	ctx := newLaunchTabBrowser(t)
	b := New(context.Background(), nil, &config.RuntimeConfig{})
	b.StealthBundle.Script += "\nwindow.__launchStealthRuns = (window.__launchStealthRuns || 0) + 1;"
	b.BrowserCtx = ctx
	tm := NewTabManager(ctx, b.Config, b.IdMgr, b.LogStore, b.tabSetup)
	b.TabManager = tm
	launchID := chromedp.FromContext(ctx).Target.TargetID
	var scriptID page.ScriptIdentifier
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		scriptID, err = page.AddScriptToEvaluateOnNewDocument(b.StealthBundle.Script).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("launch stealth setup: %v", err)
	}
	tm.SetLaunchTarget(launchID, scriptID)
	const url = "data:text/html,launch-tab"
	id, _, _, err := tm.CreateTab(url)
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	targets, err := tm.ListTargets()
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].TargetID != launchID || targets[0].URL != url {
		t.Fatalf("targets after CreateTab = %+v, want only navigated launch page", targets)
	}
	if err := tm.CloseTab(id); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	targets, err = tm.ListTargets()
	if err != nil {
		t.Fatalf("ListTargets after close: %v", err)
	}
	if len(targets) != 1 || targets[0].TargetID != launchID || targets[0].URL != "about:blank" {
		t.Fatalf("targets after CloseTab = %+v, want one blank page", targets)
	}
	assertLaunchBrowserSession(t, ctx)
	spareID := targets[0].TargetID
	var tabCtx context.Context
	id, tabCtx, _, err = tm.CreateTab(url)
	if err != nil {
		t.Fatalf("next CreateTab: %v", err)
	}
	targets, err = tm.ListTargets()
	if err != nil {
		t.Fatalf("ListTargets after reuse: %v", err)
	}
	if len(targets) != 1 || targets[0].TargetID != spareID || targets[0].URL != url {
		t.Fatalf("targets after reuse = %+v, want only navigated spare", targets)
	}
	var message string
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`console.log("re-adopted"); "ok"`, &message)); err != nil {
		t.Fatalf("evaluate after re-adoption: %v", err)
	}
	if errs := b.LogStore.GetErrorLogs(string(launchID), 0); len(errs) != 0 {
		t.Fatalf("JavaScript errors after launch page re-adoption: %+v", errs)
	}
	var injections int
	if err := chromedp.Run(tabCtx, chromedp.Evaluate("window.__launchStealthRuns", &injections)); err != nil || injections != 1 {
		t.Fatalf("stealth injections after re-adoption = %d, err = %v; want 1", injections, err)
	}
	if logs := b.LogStore.GetConsoleLogs(string(launchID), 0); len(logs) != 1 {
		t.Fatalf("console logs after re-adoption = %d, want one active listener", len(logs))
	}
	if err := tm.CloseTab(id); err != nil {
		t.Fatalf("CloseTab after reuse: %v", err)
	}
	assertLaunchBrowserSession(t, ctx)
}

func assertLaunchBrowserSession(t *testing.T, browserCtx context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(browserCtx, 2*time.Second)
	defer cancel()
	var result int
	if err := chromedp.Run(ctx, chromedp.Evaluate("1+1", &result)); err != nil {
		t.Fatalf("browser session after launch page close: %v", err)
	}
	if result != 2 {
		t.Fatalf("browser session evaluation = %d, want 2", result)
	}
}

func TestLaunchTargetSurvivesEviction(t *testing.T) {
	for _, policy := range []string{"close_lru", "close_oldest"} {
		t.Run(policy, func(t *testing.T) {
			ctx := newLaunchTabBrowser(t)
			tm := NewTabManager(ctx, &config.RuntimeConfig{MaxTabs: 1, TabEvictionPolicy: policy}, nil, nil, nil)
			launchID := chromedp.FromContext(ctx).Target.TargetID
			tm.SetLaunchTarget(launchID, "")
			id, firstCtx, _, err := tm.CreateTab("data:text/html,first")
			if err != nil {
				t.Fatalf("first CreateTab: %v", err)
			}
			nextID, _, _, err := tm.CreateTab("data:text/html,second")
			if err != nil {
				t.Fatalf("CreateTab with eviction: %v", err)
			}
			if nextID != id || firstCtx.Err() == nil || len(tm.tabs) != 1 {
				t.Fatalf("eviction = %q, want recycled launch tab %q and canceled old context", nextID, id)
			}
			targets, err := tm.ListTargets()
			if err != nil {
				t.Fatalf("ListTargets: %v", err)
			}
			if len(targets) != 1 || targets[0].TargetID != launchID || targets[0].URL != "data:text/html,second" {
				t.Fatalf("targets after eviction = %+v, want reused launch page", targets)
			}
			assertLaunchBrowserSession(t, ctx)
		})
	}
}

func TestDownloadAfterLaunchTargetClose(t *testing.T) {
	ctx := newLaunchTabBrowser(t)
	const body = "download after launch tab close"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	b := New(context.Background(), nil, &config.RuntimeConfig{})
	b.BrowserCtx = ctx
	b.TabManager = NewTabManager(ctx, b.Config, b.IdMgr, b.LogStore, nil)
	b.SetLaunchTarget(chromedp.FromContext(ctx).Target.TargetID, "")
	id, _, _, err := b.CreateTab("about:blank")
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	if err := b.CloseTab(id); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	downloadCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := b.DownloadURL(downloadCtx, server.URL, DownloadOpts{})
	if err != nil {
		t.Fatalf("DownloadURL after close: %v", err)
	}
	if result.StatusCode != http.StatusOK || string(result.Body) != body {
		t.Fatalf("DownloadURL = %+v, want local response body", result)
	}
	assertLaunchBrowserSession(t, ctx)
}

func TestAttachedBrowserDoesNotReuseLaunchTarget(t *testing.T) {
	wsURL, _ := startUserOwnedBrowser(t)
	alloc, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), wsURL)
	ctx, cancel := chromedp.NewContext(alloc)
	ctx, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	t.Cleanup(func() {
		cancelTimeout()
		cancel()
		cancelAlloc()
	})
	const userURL = "data:text/html,user-tab"
	if err := chromedp.Run(ctx, chromedp.Navigate(userURL)); err != nil {
		t.Fatalf("attach to browser: %v", err)
	}
	b := New(context.Background(), nil, &config.RuntimeConfig{})
	b.SetBrowserContexts(alloc, cancelAlloc, ctx, cancel)
	if b.spareTarget != "" || b.launchTarget != "" {
		t.Fatal("attached browser has launch-page reuse enabled")
	}
	originalTargets, err := b.ListTargets()
	if err != nil {
		t.Fatalf("list original targets: %v", err)
	}
	id, _, _, err := b.CreateTab("data:text/html,managed-tab")
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	execCtx, err := browserExecutorContext(ctx)
	if err != nil {
		t.Fatalf("browser executor: %v", err)
	}
	info, err := target.GetTargetInfo().WithTargetID(chromedp.FromContext(ctx).Target.TargetID).Do(execCtx)
	if err != nil {
		t.Fatalf("get user tab: %v", err)
	}
	if info.URL != userURL {
		t.Fatalf("user tab URL = %q, want %q", info.URL, userURL)
	}
	if err := b.CloseTab(id); err != nil {
		t.Fatalf("close managed tab: %v", err)
	}
	for _, original := range originalTargets {
		info, err := target.GetTargetInfo().WithTargetID(target.ID(original.TargetID)).Do(execCtx)
		if err != nil {
			t.Fatalf("get original tab %s after managed close: %v", original.TargetID, err)
		}
		if info.URL != original.URL {
			t.Fatalf("original tab %s URL = %q, want %q", original.TargetID, info.URL, original.URL)
		}
	}
}

func TestLaunchTargetRoutesWithProxyAuth(t *testing.T) {
	// Check browser availability before opening the proxy fixture's socket.
	_ = testbrowser.Path(t)
	const username, password = "proxy-user", "proxy-password"
	var authenticated atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)) {
			w.Header().Set("Proxy-Authenticate", `Basic realm="launch-tab"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		authenticated.Store(true)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, "proxy response")
	}))
	t.Cleanup(proxy.Close)
	ctx := newLaunchTabBrowser(t, chromedp.ProxyServer(proxy.URL))
	cfg := &config.RuntimeConfig{Proxy: config.BrowserProxyConfig{
		Server: proxy.URL, Username: username, Password: password,
	}}
	b := New(context.Background(), nil, cfg)
	b.BrowserCtx = ctx
	// Match launch-time setup before tabSetup installs it on the adopted session.
	var scriptID page.ScriptIdentifier
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		scriptID, err = page.AddScriptToEvaluateOnNewDocument(b.StealthBundle.Script).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("launch stealth setup: %v", err)
	}
	b.applyTargetStealth(ctx)
	if err := bridgeruntime.EnableProxyAuth(ctx, cfg.Proxy, nil); err != nil {
		t.Fatalf("launch proxy auth: %v", err)
	}
	const routedURL = "http://launch-tab.test/routed"
	tm := NewTabManager(ctx, cfg, b.IdMgr, b.LogStore, func(ctx context.Context, id string) error {
		if err := b.tabSetup(ctx, id); err != nil {
			return err
		}
		return b.routeMgr.AddRule(ctx, id, RouteRule{
			Pattern: routedURL, Action: RouteActionFulfill,
			Body: "routed response", ContentType: "text/plain",
		})
	})
	b.TabManager = tm
	tm.SetRouteManager(b.routeMgr)
	launchID := chromedp.FromContext(ctx).Target.TargetID
	tm.SetLaunchTarget(launchID, scriptID)
	id, tabCtx, _, err := tm.CreateTab(routedURL)
	if err != nil {
		t.Fatalf("CreateTab with route and proxy auth: %v", err)
	}
	if id != b.IdMgr.TabIDFromCDPTarget(string(launchID)) {
		t.Fatalf("CreateTab = %q, want adopted launch page", id)
	}
	var body string
	if err := chromedp.Run(tabCtx, chromedp.Text("body", &body)); err != nil {
		t.Fatalf("read routed response: %v", err)
	}
	if body != "routed response" {
		t.Fatalf("routed response = %q, want route fulfillment", body)
	}
	if err := chromedp.Run(tabCtx,
		chromedp.Navigate("http://launch-tab.test/auth"), chromedp.Text("body", &body),
	); err != nil {
		t.Fatalf("navigate through authenticated proxy: %v", err)
	}
	if !authenticated.Load() || body != "proxy response" {
		t.Fatalf("authenticated = %v, response = %q; want authenticated proxy response", authenticated.Load(), body)
	}
	if errs := b.LogStore.GetErrorLogs(string(launchID), 0); len(errs) != 0 {
		t.Fatalf("JavaScript errors after launch page adoption: %+v", errs)
	}
}
