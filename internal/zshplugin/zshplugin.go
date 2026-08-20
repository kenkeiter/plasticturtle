// Package zshplugin embeds the shell integration printed by `zsh-hook`.
//
// The plugin's one hard constraint: it runs on every directory change, so it
// must never be noticeable. It no-ops silently when the binary is absent from
// PATH, and the `_check-trust` call it makes is budgeted at
// ptcfg.CheckTrustBudget.
package zshplugin

import (
	_ "embed"
	"strings"

	"github.com/kenkeiter/plasticturtle/internal/progname"
)

//go:embed plasticturtle.plugin.zsh
var script string

// progPlaceholder is substituted with the invoked command name when the plugin
// is printed. The binary installs as `plasticturtle` and is symlinked to
// `turtle`, so the name baked into a user's ~/.zshrc should be whichever one
// they ran `zsh-hook` with — that is the one they have on PATH, and the one
// the warning should tell them to type.
const progPlaceholder = "@@PROG@@"

// Script returns the plugin source, for `source <(plasticturtle zsh-hook)`.
func Script() string {
	return strings.ReplaceAll(script, progPlaceholder, progname.Get())
}

// Exit codes for `_check-trust`, consumed by the plugin. These are a public
// contract with a shell script, so they are named here rather than inlined.
const (
	// ExitTrusted means the config is allowed at its current bytes.
	ExitTrusted = 0

	// ExitError means the check itself failed.
	ExitError = 1

	// ExitUntrusted means the config is new, changed, or invalid — the plugin
	// warns.
	ExitUntrusted = 10
)
