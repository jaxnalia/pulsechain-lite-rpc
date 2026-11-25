# Source from a deploy script, then call require_docker:
#   . "$(dirname "$0")/require-docker.sh"; require_docker
# Makes sure the Docker engine is reachable (starting Docker Desktop on
# Windows/macOS if it is installed but not running) and exits with a plain
# message otherwise.

docker_fail() {
  printf '\n%s\n' "$1" >&2
  exit 1
}

docker_ready() {
  docker info >/dev/null 2>&1
}

require_docker() {
  wait_s=${1:-180}
  command -v docker >/dev/null 2>&1 ||
    docker_fail "Docker is not installed. Install Docker (https://www.docker.com/products/docker-desktop/), start it, and try again."
  if ! docker_ready; then
    started=""
    case "$(uname -s)" in
      MINGW* | MSYS* | CYGWIN*)
        for exe in "$(cygpath -u "${PROGRAMFILES:-C:\\Program Files}" 2>/dev/null)/Docker/Docker/Docker Desktop.exe" \
          "$(cygpath -u "${LOCALAPPDATA:-}" 2>/dev/null)/Programs/DockerDesktop/Docker Desktop.exe"; do
          if [ -f "$exe" ]; then
            cmd //c start "" "$(cygpath -w "$exe")" >/dev/null 2>&1 && started=1
            break
          fi
        done ;;
      Darwin)
        open -a Docker >/dev/null 2>&1 && started=1 ;;
      *)
        docker_fail "Docker is not running. Please start Docker (for example: sudo systemctl start docker) and try again." ;;
    esac
    [ -n "$started" ] || docker_fail "Docker is not running. Please start Docker and try again."
    echo "Docker is not running. Starting Docker Desktop (this can take a minute)..."
    end=$(($(date +%s) + wait_s))
    until docker_ready; do
      [ "$(date +%s)" -lt "$end" ] ||
        docker_fail "Unable to start Docker. Please start Docker Desktop, wait until it shows 'Engine running', and try again."
      sleep 3
    done
    echo "Docker is running."
  fi
  docker compose version >/dev/null 2>&1 ||
    docker_fail "Docker Compose v2 ('docker compose') is required. Update Docker and try again."
}
