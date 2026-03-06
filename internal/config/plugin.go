package config

import (
	"errors"
	"log"
	"sync/atomic"

	ulua "github.com/micro-editor/micro/v2/internal/lua"
	lua "github.com/yuin/gopher-lua"
	luar "layeh.com/gopher-luar"
)

// ErrNoSuchFunction is returned when Call is executed on a function that does not exist
var ErrNoSuchFunction = errors.New("No such function exists")

var pluginRuntimeEnabled atomic.Bool

func init() {
	pluginRuntimeEnabled.Store(true)
}

// SetPluginRuntimeEnabled toggles lua/plugin callbacks globally for the process.
func SetPluginRuntimeEnabled(enabled bool) {
	pluginRuntimeEnabled.Store(enabled)
}

// PluginRuntimeEnabled reports whether plugin/lua callbacks are active.
func PluginRuntimeEnabled() bool {
	return pluginRuntimeEnabled.Load()
}

// LoadAllPlugins loads all detected plugins (in runtime/plugins and ConfigDir/plugins)
func LoadAllPlugins() error {
	if !PluginRuntimeEnabled() {
		return nil
	}
	ulua.Lock()
	defer ulua.Unlock()
	var reterr error
	for _, p := range Plugins {
		err := p.loadLocked()
		if err != nil {
			reterr = err
		}
	}
	return reterr
}

// RunPluginFn runs a given function in all plugins
// returns an error if any of the plugins had an error
func RunPluginFn(fn string, args ...lua.LValue) error {
	if !PluginRuntimeEnabled() {
		return nil
	}
	ulua.Lock()
	defer ulua.Unlock()
	return runPluginFnLocked(fn, args...)
}

// RunPluginFnAny runs a given function in all plugins, converting Go values
// to lua values while holding the global Lua VM lock.
func RunPluginFnAny(fn string, args ...any) error {
	if !PluginRuntimeEnabled() {
		return nil
	}
	ulua.Lock()
	defer ulua.Unlock()
	return runPluginFnLocked(fn, luaArgsFromAnyLocked(args...)...)
}

func runPluginFnLocked(fn string, args ...lua.LValue) error {
	var reterr error
	for _, p := range Plugins {
		if !p.IsLoaded() {
			continue
		}
		_, err := p.callLocked(fn, args...)
		if err != nil && err != ErrNoSuchFunction {
			reterr = errors.New("Plugin " + p.Name + ": " + err.Error())
		}
	}
	return reterr
}

// RunPluginFnBool runs a function in all plugins and returns
// false if any one of them returned false
// also returns an error if any of the plugins had an error
func RunPluginFnBool(settings map[string]any, fn string, args ...lua.LValue) (bool, error) {
	if !PluginRuntimeEnabled() {
		return true, nil
	}
	ulua.Lock()
	defer ulua.Unlock()
	return runPluginFnBoolLocked(settings, fn, args...)
}

// RunPluginFnBoolAny runs a function in all plugins and converts Go values
// to lua values while holding the global Lua VM lock.
func RunPluginFnBoolAny(settings map[string]any, fn string, args ...any) (bool, error) {
	if !PluginRuntimeEnabled() {
		return true, nil
	}
	ulua.Lock()
	defer ulua.Unlock()
	return runPluginFnBoolLocked(settings, fn, luaArgsFromAnyLocked(args...)...)
}

func runPluginFnBoolLocked(settings map[string]any, fn string, args ...lua.LValue) (bool, error) {
	var reterr error
	retbool := true
	for _, p := range Plugins {
		if !p.IsLoaded() || (settings != nil && settings[p.Name] == false) {
			continue
		}
		val, err := p.callLocked(fn, args...)
		if err == ErrNoSuchFunction {
			continue
		}
		if err != nil {
			reterr = errors.New("Plugin " + p.Name + ": " + err.Error())
			continue
		}
		if v, ok := val.(lua.LBool); ok {
			retbool = retbool && bool(v)
		}
	}
	return retbool, reterr
}

// Plugin stores information about the source files/info for a plugin
type Plugin struct {
	DirName string        // name of plugin folder
	Name    string        // name of plugin
	Info    *PluginInfo   // json file containing info
	Srcs    []RuntimeFile // lua files
	Loaded  bool
	Builtin bool
}

// IsLoaded returns if a plugin is enabled
func (p *Plugin) IsLoaded() bool {
	if !PluginRuntimeEnabled() {
		return false
	}
	if v, ok := GlobalSettings[p.Name]; ok {
		return v.(bool) && p.Loaded
	}
	return true
}

// Plugins is a list of all detected plugins (enabled or disabled)
var Plugins []*Plugin

// Load creates an option for the plugin and runs all source files
func (p *Plugin) Load() error {
	if !PluginRuntimeEnabled() {
		return nil
	}
	ulua.Lock()
	defer ulua.Unlock()
	return p.loadLocked()
}

func (p *Plugin) loadLocked() error {
	if v, ok := GlobalSettings[p.Name]; ok && !v.(bool) {
		return nil
	}
	for _, f := range p.Srcs {
		dat, err := f.Data()
		if err != nil {
			return err
		}
		err = ulua.LoadFile(p.Name, f.Name(), dat)
		if err != nil {
			return err
		}
	}
	p.Loaded = true
	RegisterCommonOption(p.Name, true)
	return nil
}

// Call calls a given function in this plugin
func (p *Plugin) Call(fn string, args ...lua.LValue) (lua.LValue, error) {
	if !PluginRuntimeEnabled() {
		return nil, nil
	}
	ulua.Lock()
	defer ulua.Unlock()
	return p.callLocked(fn, args...)
}

// CallAny converts Go arguments to lua values and calls a plugin function
// while holding the global Lua VM lock.
func (p *Plugin) CallAny(fn string, args ...any) (lua.LValue, error) {
	if !PluginRuntimeEnabled() {
		return nil, nil
	}
	ulua.Lock()
	defer ulua.Unlock()
	return p.callLocked(fn, luaArgsFromAnyLocked(args...)...)
}

func (p *Plugin) callLocked(fn string, args ...lua.LValue) (lua.LValue, error) {
	plug := ulua.L.GetGlobal(p.Name)
	if plug == lua.LNil {
		log.Println("Plugin does not exist:", p.Name, "at", p.DirName, ":", p)
		return nil, nil
	}
	luafn := ulua.L.GetField(plug, fn)
	if luafn == lua.LNil {
		return nil, ErrNoSuchFunction
	}
	err := ulua.L.CallByParam(lua.P{
		Fn:      luafn,
		NRet:    1,
		Protect: true,
	}, args...)
	if err != nil {
		return nil, err
	}
	ret := ulua.L.Get(-1)
	ulua.L.Pop(1)
	return ret, nil
}

func luaArgsFromAnyLocked(args ...any) []lua.LValue {
	if len(args) == 0 {
		return nil
	}
	largs := make([]lua.LValue, 0, len(args))
	for _, arg := range args {
		if lv, ok := arg.(lua.LValue); ok {
			largs = append(largs, lv)
			continue
		}
		largs = append(largs, luar.New(ulua.L, arg))
	}
	return largs
}

// FindPlugin returns the plugin with the given name
func FindPlugin(name string) *Plugin {
	if !PluginRuntimeEnabled() {
		return nil
	}
	var pl *Plugin
	for _, p := range Plugins {
		if !p.IsLoaded() {
			continue
		}
		if p.Name == name {
			pl = p
			break
		}
	}
	return pl
}
