// Package zshplugin embeds the shell integration printed by pt zsh-hook.
//
// The plugin's one hard constraint: it runs on every directory change, so it
// must never be noticeable. It no-ops silently when pt is absent from PATH,
// and the pt _check-trust call it makes is budgeted at ptcfg.CheckTrustBudget.
package zshplugin

import _ "embed"

//go:embed pt.plugin.zsh
var script string

// Script returns the plugin source, for `source <(pt zsh-hook)`.
func Script() string { return script }

// Exit codes for pt _check-trust, consumed by the plugin. These are a public
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
