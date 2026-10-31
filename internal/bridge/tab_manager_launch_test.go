package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/pinchtab/pinchtab/internal/config"
)

func newLaunchTabTestManager(t *testing.T, info *target.Info, setup TabSetupFunc) (*TabManager, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var created, removed atomic.Int32
	var shuttingDown atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		targets := make(map[target.ID]*target.Info)
		if info != nil {
			targets[info.TargetID] = info
		}
		var session int
		sessions := make(map[string]target.ID)
		var scripts int
		for {
			payload, _, err := wsutil.ReadClientData(conn)
			if err != nil {
				return
			}
			var command struct {
				ID        int64  `json:"id"`
				Method    string `json:"method"`
				SessionID string `json:"sessionId"`
				Params    struct {
					TargetID   target.ID `json:"targetId"`
					Identifier string    `json:"identifier"`
					URL        string    `json:"url"`
				} `json:"params"`
			}
			if json.Unmarshal(payload, &command) != nil {
				return
			}
			result := `{}`
			errorResponse := ""
			events := ""
			switch command.Method {
			case "Page.addScriptToEvaluateOnNewDocument":
				scripts++
				result = fmt.Sprintf(`{"identifier":"tab-stealth-%d"}`, scripts)
			case "Page.removeScriptToEvaluateOnNewDocument":
				removed.Add(1)
				if !strings.HasPrefix(command.Params.Identifier, "tab-stealth-") && command.SessionID != "session-1" {
					t.Errorf("remove launch script used session %q, want launch session", command.SessionID)
				}
				if command.Params.Identifier == "missing-stealth" {
					errorResponse = `,"error":{"code":-32000,"message":"Script not found"}`
				} else if command.Params.Identifier != "launch-stealth" && !strings.HasPrefix(command.Params.Identifier, "tab-stealth-") {
					t.Errorf("remove launch script identifier = %q, want launch-stealth", command.Params.Identifier)
				}
			case "Target.attachToTarget":
				session++
				sessions[fmt.Sprintf("session-%d", session)] = command.Params.TargetID
				result = fmt.Sprintf(`{"sessionId":"session-%d"}`, session)
			case "Target.getTargetInfo":
				if command.SessionID != "" {
					t.Error("Target.getTargetInfo used a page session")
				}
				if tg := targets[command.Params.TargetID]; tg != nil {
					raw, _ := json.Marshal(tg)
					result = `{"targetInfo":` + string(raw) + `}`
				} else {
					errorResponse = `,"error":{"code":-32602,"message":"No target with given id found"}`
				}
			case "Target.getTargets":
				infos := make([]*target.Info, 0, len(targets))
				for _, tg := range targets {
					infos = append(infos, tg)
				}
				raw, _ := json.Marshal(infos)
				result = `{"targetInfos":` + string(raw) + `}`
			case "Target.createTarget":
				if command.SessionID != "" {
					t.Error("Target.createTarget used a page session")
				}
				id := target.ID(fmt.Sprintf("created-%d", created.Add(1)))
				targets[id] = &target.Info{TargetID: id, Type: "page", URL: "about:blank"}
				result = fmt.Sprintf(`{"targetId":%q}`, id)
			case "Target.closeTarget":
				if command.Params.TargetID == "launch-tab" && !shuttingDown.Load() {
					t.Error("Target.closeTarget closed the launch page")
				}
				delete(targets, command.Params.TargetID)
			case "Runtime.evaluate":
				result = `{"result":{"type":"object","className":"Window"}}`
			case "Page.getFrameTree":
				result = `{"frameTree":{"frame":{"id":"frame-existing","url":"about:blank"}}}`
			case "DOM.getDocument":
				result = `{"root":{"nodeId":1,"backendNodeId":1,"nodeType":9,"nodeName":"#document","localName":"","nodeValue":""}}`
			case "Page.navigate":
				result = `{"frameId":"frame-existing","errorText":"net::ERR_FAILED"}`
				if command.Params.URL == "about:blank" {
					if tg := targets[sessions[command.SessionID]]; tg != nil {
						tg.URL = command.Params.URL
					}
					result = `{"frameId":"frame-existing"}`
					events = fmt.Sprintf(`{"method":"Page.navigatedWithinDocument","params":{"frameId":"frame-existing","url":"about:blank","navigationType":"other"},"sessionId":%q}`, command.SessionID)
				}
			}
			response := fmt.Sprintf(`{"id":%d,"result":%s%s`, command.ID, result, errorResponse)
			if command.SessionID != "" {
				response += fmt.Sprintf(`,"sessionId":%q`, command.SessionID)
			}
			if wsutil.WriteServerMessage(conn, ws.OpText, []byte(response+`}`)) != nil {
				return
			}
			if events != "" && wsutil.WriteServerMessage(conn, ws.OpText, []byte(events)) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/devtools/browser/test"
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(ctx, wsURL, chromedp.NoModifyURL)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithTargetID("launch-tab"))
	t.Cleanup(func() {
		shuttingDown.Store(true)
		browserCancel()
		allocCancel()
		cancel()
	})
	if err := chromedp.Run(browserCtx); err != nil {
		t.Fatalf("initialize browser context: %v", err)
	}
	tm := NewTabManager(browserCtx, nil, nil, nil, setup)
	tm.SetLaunchTarget("launch-tab", "launch-stealth")
	return tm, &created, &removed
}

func TestCreateTabReusesLaunchTarget(t *testing.T) {
	var setups atomic.Int32
	var removed *atomic.Int32
	tm, created, removed := newLaunchTabTestManager(t, &target.Info{
		TargetID: "launch-tab", Type: "page", URL: "about:blank",
	}, func(ctx context.Context, _ string) error {
		if c := chromedp.FromContext(ctx); c == nil || c.Target == nil {
			return fmt.Errorf("setup has no attached target")
		}
		if removed.Load() != 1 {
			return fmt.Errorf("launch script removed %d times before setup, want 1", removed.Load())
		}
		setups.Add(1)
		return nil
	})
	id, _, _, err := tm.CreateTab("about:blank")
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	if want := tm.idMgr.TabIDFromCDPTarget("launch-tab"); id != want || created.Load() != 0 {
		t.Fatalf("first CreateTab = %q, created %d targets; want %q and no new target", id, created.Load(), want)
	}
	if _, _, _, err := tm.CreateTab("about:blank"); err != nil {
		t.Fatalf("second CreateTab: %v", err)
	}
	if created.Load() != 1 {
		t.Fatalf("created %d targets, want 1", created.Load())
	}
	if setups.Load() != 2 {
		t.Fatalf("setup ran %d times, want 2", setups.Load())
	}
	if removed.Load() != 1 {
		t.Fatalf("launch script removed %d times, want 1", removed.Load())
	}
}

func TestCreateTabContinuesAfterLaunchScriptRemovalError(t *testing.T) {
	var setups atomic.Int32
	tm, created, removed := newLaunchTabTestManager(t, &target.Info{
		TargetID: "launch-tab", Type: "page", URL: "about:blank",
	}, func(context.Context, string) error {
		setups.Add(1)
		return nil
	})
	tm.SetLaunchTarget("launch-tab", "missing-stealth")
	for i := 0; i < 2; i++ {
		if _, _, _, err := tm.CreateTab("about:blank"); err != nil {
			t.Fatalf("CreateTab after removal error: %v", err)
		}
	}
	if removed.Load() != 1 || setups.Load() != 2 || created.Load() != 1 {
		t.Fatalf("removed %d, setups %d, created %d; want 1, 2, 1", removed.Load(), setups.Load(), created.Load())
	}
}

func TestCreateTabRejectsUnsuitableLaunchTarget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		info    *target.Info
		tracked bool
	}{
		{name: "navigated", info: &target.Info{TargetID: "launch-tab", Type: "page", URL: "https://example.com"}},
		{name: "tracked", info: &target.Info{TargetID: "launch-tab", Type: "page", URL: "about:blank"}, tracked: true},
		{name: "gone"},
		{name: "worker", info: &target.Info{TargetID: "launch-tab", Type: "worker", URL: "about:blank"}},
		{name: "opener", info: &target.Info{TargetID: "launch-tab", Type: "page", URL: "about:blank", OpenerID: "user-tab"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm, created, removed := newLaunchTabTestManager(t, tc.info, nil)
			launchID := tm.idMgr.TabIDFromCDPTarget("launch-tab")
			if tc.tracked {
				tm.tabs[launchID] = &TabEntry{CDPID: "launch-tab"}
			}
			id, _, _, err := tm.CreateTab("about:blank")
			if err != nil {
				t.Fatalf("CreateTab: %v", err)
			}
			if id == launchID || created.Load() != 1 {
				t.Fatalf("CreateTab = %q, created %d targets; want new target", id, created.Load())
			}
			if removed.Load() != 0 {
				t.Fatal("fallback removed the launch script")
			}
		})
	}
}

func TestCreateTabInBrowserContextPreservesLaunchTarget(t *testing.T) {
	tm, created, removed := newLaunchTabTestManager(t, &target.Info{
		TargetID: "launch-tab", Type: "page", URL: "about:blank",
	}, nil)
	if _, _, _, err := tm.CreateTabInBrowserContext("about:blank", "isolated"); err != nil {
		t.Fatalf("CreateTabInBrowserContext: %v", err)
	}
	if removed.Load() != 0 {
		t.Fatal("isolated tab removed the launch script")
	}
	id, _, _, err := tm.CreateTab("about:blank")
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	if want := tm.idMgr.TabIDFromCDPTarget("launch-tab"); id != want || created.Load() != 1 {
		t.Fatalf("CreateTab = %q, created %d targets; want spare %q", id, created.Load(), want)
	}
}

func TestParallelCreateTabClaimsLaunchTargetOnce(t *testing.T) {
	tm, created, removed := newLaunchTabTestManager(t, &target.Info{
		TargetID: "launch-tab", Type: "page", URL: "about:blank",
	}, nil)
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			id, _, _, err := tm.CreateTab("about:blank")
			results <- result{id, err}
		}()
	}
	close(start)
	adopted := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("CreateTab: %v", r.err)
		}
		if r.id == tm.idMgr.TabIDFromCDPTarget("launch-tab") {
			adopted++
		}
	}
	if adopted != 1 || created.Load() != 1 {
		t.Fatalf("adopted %d, created %d targets; want 1 each", adopted, created.Load())
	}
	if removed.Load() != 1 {
		t.Fatalf("launch script removed %d times, want 1", removed.Load())
	}
}

func TestCloseLastLaunchTargetLeavesReusableSpare(t *testing.T) {
	tm, created, removed := newLaunchTabTestManager(t, &target.Info{
		TargetID: "launch-tab", Type: "page", URL: "about:blank",
	}, nil)
	for i := 0; i < 2; i++ {
		id, _, _, err := tm.CreateTab("about:blank")
		if err != nil {
			t.Fatalf("CreateTab: %v", err)
		}
		if err := tm.CloseTab(id); err != nil {
			t.Fatalf("CloseTab: %v", err)
		}
		targets, err := tm.ListTargets()
		if err != nil {
			t.Fatalf("ListTargets: %v", err)
		}
		if len(targets) != 1 || targets[0].URL != "about:blank" || targets[0].TargetID != "launch-tab" || tm.spareTarget != tm.launchTarget {
			t.Fatalf("targets after CloseTab = %+v, want one reusable blank page", targets)
		}
	}
	if created.Load() != 0 {
		t.Fatalf("created %d targets, want no replacement pages", created.Load())
	}
	if removed.Load() != 1 {
		t.Fatalf("launch script removed %d times, want only initial removal", removed.Load())
	}
}

func TestCloseLastTabWithoutLaunchTargetStillRefuses(t *testing.T) {
	tm, created, removed := newLaunchTabTestManager(t, &target.Info{
		TargetID: "launch-tab", Type: "page", URL: "about:blank",
	}, nil)
	tm.SetLaunchTarget("", "")
	id := tm.idMgr.TabIDFromCDPTarget("launch-tab")
	tm.tabs[id] = &TabEntry{CDPID: "launch-tab"}
	if err := tm.CloseTab(id); !errors.Is(err, ErrCannotCloseLastTab) {
		t.Fatalf("CloseTab = %v, want ErrCannotCloseLastTab", err)
	}
	if created.Load() != 0 {
		t.Fatal("CloseTab created a spare in attach mode")
	}
	if removed.Load() != 0 {
		t.Fatal("CloseTab removed a launch script in attach mode")
	}
}

func TestFailedLaunchTargetPreservesLastPage(t *testing.T) {
	for _, stage := range []string{"setup", "navigate"} {
		t.Run(stage, func(t *testing.T) {
			setup := func(context.Context, string) error { return nil }
			url := "https://example.com"
			if stage == "setup" {
				url = "about:blank"
				setup = func(context.Context, string) error { return fmt.Errorf("setup failed") }
			}
			tm, _, _ := newLaunchTabTestManager(t, &target.Info{
				TargetID: "launch-tab", Type: "page", URL: "about:blank",
			}, setup)
			if _, _, _, err := tm.CreateTab(url); err == nil {
				t.Fatal("CreateTab succeeded, want failure")
			}
			targets, err := tm.ListTargets()
			if err != nil {
				t.Fatalf("ListTargets: %v", err)
			}
			if len(targets) != 1 || targets[0].URL != "about:blank" || targets[0].TargetID != "launch-tab" || tm.spareTarget != tm.launchTarget {
				t.Fatalf("targets after failed CreateTab = %+v, want original blank launch page", targets)
			}
			tm.onTabSetup = nil
			id, _, _, err := tm.CreateTab("about:blank")
			if err != nil {
				t.Fatalf("retry CreateTab: %v", err)
			}
			if want := tm.idMgr.TabIDFromCDPTarget(string(targets[0].TargetID)); id != want {
				t.Fatalf("retry CreateTab = %q, want spare %q", id, want)
			}
		})
	}
}

func TestCloseLaunchTargetCleansStateAndScripts(t *testing.T) {
	tm, _, removed := newLaunchTabTestManager(t, &target.Info{
		TargetID: "launch-tab", Type: "page", URL: "about:blank",
	}, nil)
	tm.SetNetworkMonitor(NewNetworkMonitor(0))
	tm.SetRouteManager(NewRouteManager(nil))
	tm.logStore = NewConsoleLogStore(10)
	var hooks int
	tm.AddTabRemovedHook(func(string) { hooks++ })
	tm.onTabSetup = func(ctx context.Context, _ string) error {
		id, err := page.AddScriptToEvaluateOnNewDocument("window.stealth = true").Do(ctx)
		if err == nil {
			tm.rememberLaunchScript(ctx, id)
		}
		return err
	}
	for i := 0; i < 2; i++ {
		id, ctx, _, err := tm.CreateTab("about:blank")
		if err != nil {
			t.Fatalf("CreateTab: %v", err)
		}
		if err := tm.routeMgr.AddRule(ctx, id, RouteRule{Pattern: "*", Action: RouteActionContinue}); err != nil {
			t.Fatalf("AddRule: %v", err)
		}
		tm.SetRefCache(id, &RefCache{})
		if err := tm.CloseTab(id); err != nil {
			t.Fatalf("CloseTab: %v", err)
		}
		if ctx.Err() == nil || tm.browserCtx.Err() != nil || tm.launchCtx.Err() != nil {
			t.Fatal("close must cancel only the managed context")
		}
		if len(tm.tabs) != 0 || len(tm.accessed) != 0 || tm.currentTab != "" || tm.GetRefCache(id) != nil {
			t.Fatal("close retained managed tab state")
		}
		if tm.netMonitor.GetBuffer(id) != nil || len(tm.routeMgr.List(id)) != 0 || len(tm.launchScripts) != 0 {
			t.Fatal("close retained capture, route, or script state")
		}
	}
	if hooks != 2 || removed.Load() != 3 {
		t.Fatalf("hooks = %d, removed scripts = %d; want 2, 3", hooks, removed.Load())
	}
}

func TestCloseNonLaunchTargetStillClosesPage(t *testing.T) {
	tm, created, _ := newLaunchTabTestManager(t, &target.Info{
		TargetID: "launch-tab", Type: "page", URL: "about:blank",
	}, nil)
	id, ctx, _, err := tm.CreateTabInBrowserContext("about:blank", "isolated")
	if err != nil {
		t.Fatalf("CreateTabInBrowserContext: %v", err)
	}
	if err := tm.CloseTab(id); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	targets, err := tm.ListTargets()
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if ctx.Err() == nil || len(targets) != 1 || targets[0].TargetID != "launch-tab" || created.Load() != 1 {
		t.Fatalf("close retained non-launch page: %+v", targets)
	}
}

func TestLaunchTargetSurvivesUnmanagedClosePaths(t *testing.T) {
	tm, _, _ := newLaunchTabTestManager(t, &target.Info{
		TargetID: "launch-tab", Type: "page", URL: "about:blank",
	}, nil)
	if tm.closeTarget("launch-tab") {
		t.Fatal("tab budget closed the launch page")
	}
	tm.closePopupTarget("launch-tab", "opener", "about:blank")
	if err := tm.CloseTab("launch-tab"); err != nil {
		t.Fatalf("CloseTab by raw target: %v", err)
	}
	if tm.spareTarget != tm.launchTarget {
		t.Fatal("unmanaged close did not re-arm the launch page")
	}
}

func TestLaunchTargetIsExcludedFromOrphanBudget(t *testing.T) {
	tm := NewTabManager(context.Background(), nil, nil, nil, nil)
	tm.SetLaunchTarget("launch-tab", "")
	pages := []*target.Info{pageTarget("orphan"), pageTarget("launch-tab")}
	orphans := selectOrphanTargets(tm.trackedCDPIDs(), pages, 1)
	if len(orphans) != 1 || orphans[0] != "orphan" {
		t.Fatalf("orphan targets = %v, want only non-launch target", orphans)
	}
}

func TestLaunchTargetEvictionReusesPage(t *testing.T) {
	for _, policy := range []string{"close_lru", "close_oldest"} {
		t.Run(policy, func(t *testing.T) {
			tm, created, _ := newLaunchTabTestManager(t, &target.Info{
				TargetID: "launch-tab", Type: "page", URL: "about:blank",
			}, nil)
			tm.config = &config.RuntimeConfig{MaxTabs: 1, TabEvictionPolicy: policy}
			id, oldCtx, _, err := tm.CreateTab("about:blank")
			if err != nil {
				t.Fatalf("first CreateTab: %v", err)
			}
			next, _, _, err := tm.CreateTab("about:blank")
			if err != nil {
				t.Fatalf("CreateTab with eviction: %v", err)
			}
			if next != id || oldCtx.Err() == nil || len(tm.tabs) != 1 || created.Load() != 0 {
				t.Fatal("eviction must recycle the launch page and cancel only the old managed context")
			}
		})
	}
}

func TestFailedLaunchTargetCleansPartialSetup(t *testing.T) {
	tm, created, removed := newLaunchTabTestManager(t, &target.Info{
		TargetID: "launch-tab", Type: "page", URL: "about:blank",
	}, nil)
	tm.SetNetworkMonitor(NewNetworkMonitor(0))
	tm.SetRouteManager(NewRouteManager(nil))
	tm.onTabSetup = func(ctx context.Context, tabID string) error {
		id, err := page.AddScriptToEvaluateOnNewDocument("window.stealth = true").Do(ctx)
		if err != nil {
			return err
		}
		tm.rememberLaunchScript(ctx, id)
		if err := tm.netMonitor.StartCapture(ctx, tabID); err != nil {
			return err
		}
		if err := tm.routeMgr.AddRule(ctx, tabID, RouteRule{Pattern: "*", Action: RouteActionContinue}); err != nil {
			return err
		}
		return fmt.Errorf("setup failed after installing listeners")
	}
	if _, _, _, err := tm.CreateTab("about:blank"); err == nil {
		t.Fatal("CreateTab succeeded, want setup failure")
	}
	id := tm.idMgr.TabIDFromCDPTarget("launch-tab")
	if tm.netMonitor.GetBuffer(id) != nil || len(tm.routeMgr.List(id)) != 0 || len(tm.launchScripts) != 0 {
		t.Fatal("failed adoption retained capture, route, or script state")
	}
	if tm.spareTarget != tm.launchTarget || created.Load() != 0 || removed.Load() != 2 {
		t.Fatal("failed adoption must reset the same launch target and remove its scripts")
	}
}
