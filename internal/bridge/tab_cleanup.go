package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/chromedp/cdproto/target"
)

// closeOldestTab evicts the tab with the earliest CreatedAt timestamp.
func (tm *TabManager) closeOldestTab() error {
	tm.mu.RLock()
	var oldestID string
	var oldestTime time.Time
	for id, entry := range tm.tabs {
		if oldestID == "" || entry.CreatedAt.Before(oldestTime) {
			oldestID = id
			oldestTime = entry.CreatedAt
		}
	}
	tm.mu.RUnlock()

	if oldestID == "" {
		return fmt.Errorf("no tabs to evict")
	}
	slog.Info("evicting oldest tab", "id", oldestID, "createdAt", oldestTime)
	return tm.CloseTab(oldestID)
}

// closeLRUTab evicts the tab with the earliest LastUsed timestamp.
func (tm *TabManager) closeLRUTab() error {
	tm.mu.RLock()
	var lruID string
	var lruTime time.Time
	for id, entry := range tm.tabs {
		t := entry.LastUsed
		if t.IsZero() {
			t = entry.CreatedAt
		}
		if lruID == "" || t.Before(lruTime) {
			lruID = id
			lruTime = t
		}
	}
	tm.mu.RUnlock()

	if lruID == "" {
		return fmt.Errorf("no tabs to evict")
	}
	slog.Info("evicting LRU tab", "id", lruID, "lastUsed", lruTime)
	return tm.CloseTab(lruID)
}

func (tm *TabManager) CleanStaleTabs(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		tm.dropTabsTheBrowserNoLongerHas()
	}
}

// dropTabsTheBrowserNoLongerHas removes tracked tabs whose CDP target is gone.
//
// A tab can die without PinchTab hearing about it. Target.targetDestroyed is
// delivered over the CDP session of the tab chromedp adopted for the
// browser-level context, so once that tab dies the event stops arriving for
// every other tab too. That is routine for an attached browser, where the tabs
// belong to the user and PinchTab adopted one of them. A dead entry keeps a
// context that no longer has a session behind it, so calls on it block until
// their deadline rather than failing, and tab selection keeps choosing it.
//
// Target.getTargets is a browser-level command, so what it reports stays true
// whichever tab has died.
func (tm *TabManager) dropTabsTheBrowserNoLongerHas() {
	if tm == nil {
		return
	}
	tm.mu.RLock()
	tracked := len(tm.tabs)
	tm.mu.RUnlock()
	if tracked == 0 {
		return
	}

	targets, err := tm.ListTargets()
	if err != nil {
		// Without a target list nothing is proven dead. Dropping the set here
		// would evict live tabs every time the browser is briefly busy.
		return
	}
	alive := make(map[string]bool, len(targets))
	for _, t := range targets {
		alive[string(t.TargetID)] = true
	}

	type staleTab struct {
		tabID string
		cdpID string
	}
	var staleTabs []staleTab
	tm.mu.RLock()
	for id, entry := range tm.tabs {
		// Only an entry that records which CDP target it belongs to can be
		// missing from the listing. RegisterTab binds a context without one,
		// and for those absence proves nothing, so they are left alone.
		if entry == nil || entry.CDPID == "" {
			continue
		}
		if !alive[entry.CDPID] {
			staleTabs = append(staleTabs, staleTab{tabID: id, cdpID: entry.CDPID})
		}
	}
	tm.mu.RUnlock()

	for _, stale := range staleTabs {
		tm.purgeTrackedTabState(stale.tabID, stale.cdpID)
		slog.Info("cleaned stale tab", "id", stale.tabID)
	}
}

func (tm *TabManager) purgeTrackedTabState(tabID, cdpTargetID string) bool {
	resolvedTabID, resolvedCDPID, cancel, ok := tm.lookupTrackedTabForCleanup(tabID, cdpTargetID)
	if !ok {
		return false
	}
	if cancel != nil {
		cancel()
	}

	tm.mu.Lock()
	if entry, ok := tm.tabs[resolvedTabID]; ok {
		entry.stopIdleTimer()
	}
	delete(tm.tabs, resolvedTabID)
	delete(tm.snapshots, resolvedTabID)
	delete(tm.frameScope, resolvedTabID)
	delete(tm.accessed, resolvedTabID)
	if tm.currentTab == resolvedTabID {
		tm.currentTab = ""
	}
	tm.mu.Unlock()

	tm.cleanupTabResources(resolvedTabID, resolvedCDPID)
	// Notify listeners (e.g. session persistence) that a tab disappeared,
	// regardless of whether the trigger was a deliberate CloseTab, an eviction,
	// the auto-close lifecycle timer, or Chrome reporting the target gone
	// (CleanStaleTabs / user closing the tab in headed Chrome directly).
	if tm.onAfterClose != nil {
		tm.onAfterClose()
	}
	return true
}

func (tm *TabManager) cleanupTabResources(resolvedTabID, resolvedCDPID string) {
	// Re-adoption reuses this ID, so release its capture slot before creating again.
	if tm.netMonitor != nil && tm.isLaunchTarget(target.ID(resolvedCDPID)) {
		tm.netMonitor.StopCapture(resolvedTabID)
	}
	if tm.dialogMgr != nil {
		tm.dialogMgr.ClearPending(resolvedTabID)
	}
	if tm.executor != nil {
		tm.executor.RemoveTab(resolvedTabID)
	}
	if tm.logStore != nil {
		tm.logStore.RemoveTab(resolvedCDPID)
	}
	if tm.routeMgr != nil {
		tm.routeMgr.RemoveTab(resolvedTabID)
	}
	// Snapshot under the lock, then invoke unlocked: a hook must be free to call
	// back into the TabManager without deadlocking on tm.mu.
	tm.mu.RLock()
	hooks := tm.onTabRemovedHooks
	tm.mu.RUnlock()
	for _, hook := range hooks {
		hook(resolvedTabID)
	}
}

func (tm *TabManager) lookupTrackedTabForCleanup(tabID, cdpTargetID string) (string, string, context.CancelFunc, bool) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if tabID != "" {
		if entry, ok := tm.tabs[tabID]; ok {
			resolvedCDPID := cdpTargetID
			if resolvedCDPID == "" {
				resolvedCDPID = entry.CDPID
			}
			if resolvedCDPID == "" {
				resolvedCDPID = tabID
			}
			return tabID, resolvedCDPID, entry.Cancel, true
		}
	}

	if cdpTargetID == "" {
		return "", "", nil, false
	}

	for id, entry := range tm.tabs {
		resolvedCDPID := entry.CDPID
		if resolvedCDPID == "" {
			resolvedCDPID = id
		}
		if id == cdpTargetID || resolvedCDPID == cdpTargetID {
			return id, resolvedCDPID, entry.Cancel, true
		}
	}
	return "", "", nil, false
}

func (tm *TabManager) purgeTrackedTabStateByTargetID(cdpTargetID string) bool {
	return tm.purgeTrackedTabState("", cdpTargetID)
}
