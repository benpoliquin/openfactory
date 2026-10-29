#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ENV_FILE=${OPENFACTORY_ENV_FILE:-$ROOT/.env}
CONFIG_FILE=${OPENFACTORY_CONFIG_FILE:-$ROOT/config/openfactory.yaml}
case "$ENV_FILE" in /*) ;; *) ENV_FILE=$ROOT/$ENV_FILE ;; esac
case "$CONFIG_FILE" in /*) ;; *) CONFIG_FILE=$ROOT/$CONFIG_FILE ;; esac
export OPENFACTORY_CONFIG_FILE=$CONFIG_FILE
export OPENFACTORY_CONFIG_DIR
OPENFACTORY_CONFIG_DIR=$(dirname -- "$CONFIG_FILE")
export OPENFACTORY_CONFIG_BASENAME
OPENFACTORY_CONFIG_BASENAME=$(basename -- "$CONFIG_FILE")

fail() { printf 'openfactory: %s\n' "$*" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || fail 'Docker is required; install Docker with the Compose plugin.'
docker compose version >/dev/null 2>&1 || fail 'The Docker Compose v2 plugin is required.'
# Newer Compose clients may otherwise request an API unsupported by an older
# daemon. Respect an explicit operator override; discover the daemon otherwise.
if [[ -z "${DOCKER_API_VERSION:-}" ]]; then
  DOCKER_API_VERSION=$(docker version --format '{{.Server.APIVersion}}') \
    || fail 'Cannot discover the Docker daemon API version.'
  [[ "$DOCKER_API_VERSION" =~ ^[0-9]+\.[0-9]+$ ]] || fail 'Docker returned an invalid API version.'
  export DOCKER_API_VERSION
fi

# Never source the operator's dotenv file as shell code. Compose parses it.
compose() (
  cd -- "$ROOT"
  docker compose --project-directory "$ROOT" --env-file "$ENV_FILE" "$@"
)

copy_if_absent() {
  local source=$1 destination=$2 mode=$3
  if [[ -e "$destination" || -L "$destination" ]]; then
    [[ -f "$destination" && -r "$destination" ]] || fail "Not a readable file: $destination"
    return
  fi
  [[ -f "$source" ]] || fail "Missing packaged example: $source"
  local parent
  parent=$(dirname -- "$destination")
  if [[ ! -d "$parent" ]]; then
    mkdir -p -- "$parent"
    # A freshly created config directory must be traversable by UID10001.
    # Never change permissions on an existing operator-owned directory.
    if [[ "$mode" == 644 ]]; then chmod 755 "$parent"; fi
  fi
  # noclobber makes concurrent initializers fail closed rather than overwrite.
  (umask 077; set -o noclobber; cat -- "$source" > "$destination") \
    || fail "Could not initialize $destination (an existing file was not overwritten)."
  chmod "$mode" "$destination"
  printf 'Created %s; review before enrolling a repository.\n' "$destination"
}

ACTION=${1:-start}
if [[ $# -gt 0 ]]; then shift; fi
case "$ACTION" in
  start|up)
    [[ $# -eq 0 ]] || fail 'Usage: ./start.sh [start|stop|logs|cli ARGS...|bb COMMAND...|compose ARGS...]'
    copy_if_absent "$ROOT/.env.example" "$ENV_FILE" 600
    copy_if_absent "$ROOT/examples/openfactory.yaml" "$CONFIG_FILE" 644
    docker info >/dev/null 2>&1 || fail 'Docker is not running or the current user cannot access it.'
    compose up --build --detach --wait
    printf '\nOpenFactory dashboard: http://127.0.0.1:38887 (read-only, loopback only)\n'
    printf 'Discovery programs: http://127.0.0.1:38887/programs (read-only)\n'
    printf 'Direct BB sessions: http://127.0.0.1:38886 (loopback; not the operator browser frontdoor)\n'
    printf 'Authenticate: ./start.sh bb omp login\n'
    printf 'Model catalog: ./start.sh bb omp models ls --json\n'
    printf 'Read-only status: ./start.sh cli status\n'
    printf 'Phone intake is disabled by default: ./start.sh cli config intake\n'
    printf 'When enabled, private browser HTTPS must target ONLY the frontdoor on 127.0.0.1:38889 (all paths intact).\n'
    printf 'Private HTTPS/enrollment example: examples/intake-private-https.txt (review; never applied by starter)\n'
    printf 'No work is created or approved by this script. Existing approved work resumes.\n'
    ;;
  stop|down)
    [[ $# -eq 0 ]] || fail 'Usage: ./start.sh stop'
    [[ -f "$ENV_FILE" ]] || fail "Missing environment file: $ENV_FILE"
    # Do not use down --volumes: state and model credentials must survive stop.
    compose stop
    ;;
  logs)
    [[ -f "$ENV_FILE" ]] || fail "Missing environment file: $ENV_FILE"
    compose logs --follow "$@"
    ;;
  cli)
    [[ -f "$ENV_FILE" ]] || fail "Missing environment file: $ENV_FILE"
    compose exec -T openfactory /usr/local/bin/openfactory --config "/config/$OPENFACTORY_CONFIG_BASENAME" "$@"
    ;;
  bb)
    [[ $# -gt 0 ]] || fail 'Usage: ./start.sh bb COMMAND [ARGS...]'
    [[ -f "$ENV_FILE" ]] || fail "Missing environment file: $ENV_FILE"
    compose exec bb "$@"
    ;;
  compose)
    [[ -f "$ENV_FILE" ]] || fail "Missing environment file: $ENV_FILE"
    compose "$@"
    ;;
  *) fail 'Usage: ./start.sh [start|stop|logs|cli ARGS...|bb COMMAND...|compose ARGS...]' ;;
esac
