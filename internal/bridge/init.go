package bridge

import (
	"context"

	"github.com/chromedp/cdproto/page"
	bridgeruntime "github.com/pinchtab/pinchtab/internal/bridge/runtime"
	"github.com/pinchtab/pinchtab/internal/config"
	"github.com/pinchtab/pinchtab/internal/stealth"
)

const popupGuardInitScript = stealth.PopupGuardInitScript

func InitBrowser(cfg *config.RuntimeConfig, bundle *stealth.Bundle) (context.Context, context.CancelFunc, context.Context, context.CancelFunc, stealth.LaunchMode, page.ScriptIdentifier, error) {
	var scriptID page.ScriptIdentifier
	allocCtx, allocCancel, browserCtx, browserCancel, launchMode, err := bridgeruntime.InitBrowser(cfg, bundle, bridgeruntime.Hooks{
		SetHumanRandSeed:           SetHumanRandSeed,
		SetLaunchStealthScript:     func(id page.ScriptIdentifier) { scriptID = id },
		IsProfileLockError:         isProfileLockError,
		ClearStaleProfileLocks:     clearStaleProfileLocks,
		ConfigureBrowserProcess:    configureBrowserProcess,
		QuarantineCorruptedProfile: quarantineCorruptedProfile,
	})
	return allocCtx, allocCancel, browserCtx, browserCancel, launchMode, scriptID, err
}

func baseBrowserFlagArgs() []string {
	return bridgeruntime.BaseBrowserFlagArgs()
}

func buildBrowserArgs(cfg *config.RuntimeConfig, port int) []string {
	return bridgeruntime.BuildBrowserArgs(cfg, port)
}
