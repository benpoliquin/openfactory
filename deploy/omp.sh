#!/bin/sh
set -eu
# This dedicated BB image owns its launch policy; never rewrite host omp config.
# Provider authentication remains in the BB-only persistent home.
if [ "${1:-}" = acp ]; then
  shift
  # Intake has no research, write, native, or ambient extension capabilities.
  intake_cwd="$(pwd -P)"
  case "$intake_cwd" in
    */intake/*/*/*/*)
      if [ "$#" -ne 0 ] || [ ! -f .openfactory/intake-session.json ] || [ -L .openfactory/intake-session.json ]; then
        printf '%s\n' 'intake_launch_rejected' >&2
        exit 1
      fi
      unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
      unset PI_CONFIG_FILES
      exec bun /usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js acp \
        --approval-mode yolo --no-title --max-time "${OPENFACTORY_AGENT_MAX_TIME:-30m}" \
        --no-extensions --no-rules --no-skills \
        --config /opt/openfactory/intake/intake-config.yaml \
        --extension /usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent/openfactory/intake-extension.ts \
        --tools intake_read
      ;;
  esac
  # Dedicated discovery sessions use a fixed image-owned overlay and exact
  # custom-tool allowlist. This does not change ordinary implementation runs
  # and is not an OS sandbox for hostile code.
  discovery_cwd="$(pwd -P)"
  case "$discovery_cwd" in
    */discovery/*/*/*/*)
      if [ ! -f .openfactory/discovery-session.json ] || [ -L .openfactory/discovery-session.json ]; then
        printf '%s\n' 'discovery_control_missing' >&2
        exit 1
      fi
      # A caller cannot append config/tool/extension switches to this policy.
      # BB's ACP transport supplies model/cwd per session, not launch flags.
      if [ "$#" -ne 0 ]; then
        printf '%s\n' 'discovery_launch_arguments_rejected' >&2
        exit 1
      fi
      unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
      unset PI_CONFIG_FILES
      exec bun /usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js acp \
        --approval-mode yolo --no-title --max-time "${OPENFACTORY_AGENT_MAX_TIME:-30m}" \
        --no-extensions --no-rules --no-skills \
        --config "${OPENFACTORY_DISCOVERY_CONFIG:-/opt/openfactory/discovery/research-config.yaml}" \
        --extension "${OPENFACTORY_RESEARCH_EXTENSION:-/usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent/openfactory/research-extension.ts}" \
        --tools research_search,research_capture,research_github,research_source,discovery_read,discovery_write
      ;;
  esac
  # Seed the default role routing once. The file lives in the persistent BB
  # home, so operator edits survive restarts and are never overwritten.
  config_dir="${HOME}/.omp/agent"
  config_file="${config_dir}/config.yml"
  if [ ! -e "$config_file" ]; then
    mkdir -p "$config_dir"
    (umask 077; cat > "$config_file" <<'YAML'
modelRoles:
  default: openai-codex/gpt-6.1-sol:high
  plan: openai-codex/gpt-6-astra:high
  slow: openai-codex/gpt-6-astra:high
  smol: openrouter/qwen/qwen3.8-flash:high
  implementation: openai-codex/gpt-6.1-sol:high
  review: openai-codex/gpt-6-astra:high
  mechanical: openrouter/z-ai/glm-5.3-flash:high

task:
  agentModelOverrides:
    task: "@implementation"
    scout: "@smol"
    sonic: "@mechanical"
    reviewer: "@review"
    security-reviewer: "@review"

advisor:
  enabled: false

bash:
  gitGuard: true
YAML
    )
  fi
  exec bun /usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js acp \
    --approval-mode yolo --no-title --max-time "${OPENFACTORY_AGENT_MAX_TIME:-30m}" \
    --tools read,bash,edit,write,grep,find,glob,eval,todo,task "$@"
fi
exec bun /usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js "$@"
