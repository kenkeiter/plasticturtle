# pt.plugin.zsh — Plastic Turtle shell integration (placeholder, wave 3).
#
# Installed with:  source <(pt zsh-hook)
#
# Wave 3 (agent J) replaces this file with the real implementation:
#   - chpwd hook: bounded upward walk for .plasticturtle, stopping at / and $HOME
#   - pt _check-trust <dir>: 0 trusted, 10 untrusted/changed, 1 error
#   - one-line yellow warning when untrusted
#   - PT_PROMPT turtle segment, green when trusted, yellow when not
#   - silent no-op when pt is not on PATH
command -v pt >/dev/null 2>&1 || return 0
