# pt.plugin.zsh — Plastic Turtle shell integration.
#
# Install by adding this to ~/.zshrc:
#
#     source <(pt zsh-hook)
#
# It warns when a project's .plasticturtle is not allowed.
#
# Three constraints shape everything below. This runs on every directory
# change, so it must never be noticeable. It lands in other people's shells, so
# it must not leak names or disturb $?. And it
# must survive options the user may have set — notably errexit, under which a
# bare non-zero exit anywhere here would kill the shell.

# No pt, no plugin. Sourcing this from .zshrc on a machine where pt is not
# installed must be silent rather than an error on every new shell.
if ! command -v pt >/dev/null 2>&1; then
  return 0
fi

# Sourcing twice must not double-register hooks. Prompt frameworks re-source
# their fragments, so this is not hypothetical.
if (( ${+_PT_PLUGIN_LOADED} )); then
  return 0
fi
typeset -g _PT_PLUGIN_LOADED=1

autoload -Uz add-zsh-hook

typeset -g _PT_PROJECT_DIR=''
typeset -g _PT_TRUST=''

# Exit codes from `pt _check-trust`, mirroring internal/zshplugin's constants.
typeset -gr _PT_EXIT_TRUSTED=0
typeset -gr _PT_EXIT_UNTRUSTED=10

# _pt_find_project walks upward from $1 (default $PWD) looking for a
# .plasticturtle, printing the containing directory.
#
# The walk stops at $HOME and at /, so a shell sitting deep in a home directory
# does not stat its way to the root on every cd.
_pt_find_project() {
  # Both ends of the $HOME comparison are symlink-resolved, once, before the
  # walk. On macOS a home or temp path routinely reaches us as /var/... while
  # $HOME is /private/var/... (or the reverse); comparing the two textually
  # never matches, and the walk then sails straight past $HOME to the root.
  local dir=${${1:-$PWD}:A}
  local home=${HOME:A}

  # $PWD can name a directory that has been deleted underneath the shell.
  if [[ ! -d $dir ]]; then
    return 1
  fi

  while true; do
    if [[ -f $dir/.plasticturtle ]]; then
      print -r -- $dir
      return 0
    fi
    if [[ $dir == $home || $dir == / ]]; then
      return 1
    fi
    local parent=${dir:h}
    if [[ $parent == $dir ]]; then
      return 1
    fi
    dir=$parent
  done
}

# _pt_chpwd decides everything. It runs once per directory change, which is
# also why the warning appears once per directory change rather than on every
# prompt redraw — a warning that reprints on each Enter is noise people mute.
_pt_chpwd() {
  # Preserve the caller's exit status: a hook that clobbers $? breaks every
  # prompt that renders the last command's status, which is most of them.
  local last=$?

  local dir
  if ! dir=$(_pt_find_project); then
    _PT_PROJECT_DIR=''
    _PT_TRUST=''
    return $last
  fi

  # Same directory, already decided: skip the subprocess. `cd .` and prompt
  # frameworks that fire chpwd spuriously are both common.
  if [[ $dir == $_PT_PROJECT_DIR && -n $_PT_TRUST ]]; then
    return $last
  fi

  _PT_PROJECT_DIR=$dir

  # The non-zero codes here are answers, not failures, so the call is written as
  # a condition: a bare `pt _check-trust; code=$?` would abort the shell under
  # errexit, and 10 — "not allowed" — is the most likely code this ever gets.
  local code=0
  pt _check-trust $dir >/dev/null 2>&1 || code=$?

  case $code in
    $_PT_EXIT_TRUSTED)
      _PT_TRUST=trusted
      ;;
    $_PT_EXIT_UNTRUSTED)
      _PT_TRUST=untrusted
      print -P -- "%F{yellow}⚠️  .plasticturtle is not allowed (new or changed). Run 'pt allow' before 'pt shell'.%f"
      ;;
    *)
      # pt failed for a reason of its own. Claiming either trust state would be
      # a lie, so say nothing rather than guess.
      _PT_TRUST=error
      ;;
  esac

  return $last
}

add-zsh-hook chpwd _pt_chpwd

# Decide for the directory the shell started in, not just for later cds.
_pt_chpwd
