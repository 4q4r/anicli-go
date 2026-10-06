package lua

import (
	lua "github.com/yuin/gopher-lua"
)

// The PR140 settings SDK: anicli.provider_setting(key) reads the
// STRING value of providers.<id>.<key> for the script's OWN provider
// id — the config-read leg of the PR116 phase-3 plan, built for the
// kodik migration (the API token). The engine carries the flattened
// map of the ONE provider being loaded (Config.ProviderSettings; the
// loader and the factory wire it per id through the same seam as the
// HTTP client), so a script is structurally unable to read another
// provider's settings.
//
// Contract:
//
//   - a configured key yields its string value verbatim;
//   - a missing key or a provider without a settings section yields
//     Lua nil — never an error: the credential-gated provider decides
//     what a missing token means (the kodik script fails loud naming
//     the settings paths);
//   - values are secret-bearing (API tokens): the read is silent —
//     nothing ever routes the value into the engine logger.
//
// The map is read-only from the script's perspective: there is no
// write surface, and the map itself belongs to the engine config.

// openSDKSettings registers anicli.provider_setting onto the SDK
// module table.
func (e *Engine) openSDKSettings(ls *lua.LState, mod *lua.LTable) {
	mod.RawSetString("provider_setting", ls.NewFunction(e.sdkProviderSetting))
}

// sdkProviderSetting implements anicli.provider_setting(key): the
// configured string value of the calling provider's settings key, or
// Lua nil when the key (or the whole section) is absent. The read is
// silent by contract — the value never reaches the engine logger.
func (e *Engine) sdkProviderSetting(ls *lua.LState) int {
	key := ls.CheckString(1)
	if v, ok := e.cfg.ProviderSettings[key]; ok {
		ls.Push(lua.LString(v))
	} else {
		ls.Push(lua.LNil)
	}
	return 1
}
