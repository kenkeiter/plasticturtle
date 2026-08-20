package main

import (
	"os"
	"path/filepath"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/trust"
	"github.com/kenkeiter/plasticturtle/internal/zshplugin"
)

// checkTrust answers "is this project's config allowed at its current bytes?"
// for the zsh plugin, returning one of zshplugin's exit codes.
//
// It runs on every directory change in the user's shell, so it does the least
// work that can answer the question: no YAML parsing, no validation, no locks,
// and no cobra. Trust is defined over the file's exact bytes, so hashing them
// and reading trust.json is sufficient — whether the config is *valid* is a
// question for plasticturtle allow and plasticturtle shell, which have a terminal to explain
// themselves on.
func checkTrust(dir string) int {
	projectDir, err := config.Find(dir)
	if err != nil {
		// Including the case where the file vanished between the plugin's own
		// check and this one. There is nothing to be trusted or distrusted, so
		// neither verdict is honest; the plugin renders nothing on an error.
		return zshplugin.ExitError
	}

	raw, err := os.ReadFile(filepath.Join(projectDir, config.FileName))
	if err != nil {
		return zshplugin.ExitError
	}

	root, err := state.DefaultRoot()
	if err != nil {
		return zshplugin.ExitError
	}
	store, err := trust.Open(filepath.Join(root, trust.FileName))
	if err != nil {
		return zshplugin.ExitError
	}

	ok, err := store.Check(projectDir, config.HashBytes(raw))
	if err != nil {
		return zshplugin.ExitError
	}
	if ok {
		return zshplugin.ExitTrusted
	}
	return zshplugin.ExitUntrusted
}
