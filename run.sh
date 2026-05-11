#!/usr/bin/env bash
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

BACKEND_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FRONTEND_DIR="${BACKEND_DIR}/frontend"
CONFIG_FILE="${BACKEND_DIR}/config.yaml"
FRONTEND_PORT="5173"

BACKEND_PID=""
FRONTEND_PID=""

cleanup() {
    echo -e "\n${YELLOW}Shutting down services...${NC}"
    if [ -n "${FRONTEND_PID}" ] && kill -0 "${FRONTEND_PID}" 2>/dev/null; then
        echo "Stopping frontend (PID: ${FRONTEND_PID})..."
        kill "${FRONTEND_PID}" 2>/dev/null || true
        wait "${FRONTEND_PID}" 2>/dev/null || true
    fi
    if [ -n "${BACKEND_PID}" ] && kill -0 "${BACKEND_PID}" 2>/dev/null; then
        echo "Stopping backend (PID: ${BACKEND_PID})..."
        kill "${BACKEND_PID}" 2>/dev/null || true
        wait "${BACKEND_PID}" 2>/dev/null || true
    fi
    echo -e "${GREEN}All services stopped.${NC}"
    exit 0
}

trap cleanup SIGINT SIGTERM EXIT

check_deps() {
    local missing=()

    if ! command -v go &>/dev/null; then
        missing+=("go (https://go.dev/dl/)")
    fi

    if ! command -v node &>/dev/null; then
        missing+=("node (https://nodejs.org/)")
    fi

    if ! command -v npm &>/dev/null; then
        missing+=("npm")
    fi

    if [ ${#missing[@]} -ne 0 ]; then
        echo -e "${RED}Missing required dependencies:${NC}"
        printf '  - %s\n' "${missing[@]}"
        exit 1
    fi
}

check_go_version() {
    local required="1.26"
    local current
    current=$(go version | awk '{print $3}' | sed 's/go//')

    if [ "$(printf '%s\n' "$required" "$current" | sort -V | head -n1)" != "$required" ]; then
        echo -e "${RED}Go version ${current} is too old. Required: >= ${required}${NC}"
        exit 1
    fi
}

setup_config() {
    if [ ! -f "${CONFIG_FILE}" ]; then
        local example="${BACKEND_DIR}/config.example.yaml"
        if [ -f "${example}" ]; then
            echo -e "${YELLOW}config.yaml not found. Copying from config.example.yaml...${NC}"
            cp "${example}" "${CONFIG_FILE}"
            echo -e "${YELLOW}Please edit ${CONFIG_FILE} with your actual credentials before restarting.${NC}"
            echo -e "${YELLOW}Continuing with example config (some features may not work)...${NC}"
        else
            echo -e "${RED}config.yaml not found and config.example.yaml is missing.${NC}"
            exit 1
        fi
    fi
}

setup_frontend() {
    if [ ! -d "${FRONTEND_DIR}/node_modules" ]; then
        echo -e "${YELLOW}Installing frontend dependencies...${NC}"
        (cd "${FRONTEND_DIR}" && npm install)
    fi
}

load_frontend_port() {
    local port
    port=$(awk '
        /^frontend:/ { in_frontend = 1; next }
        /^[^[:space:]]/ { in_frontend = 0 }
        in_frontend && /^[[:space:]]+port:/ {
            gsub(/"/, "", $2)
            print $2
            exit
        }
    ' "${CONFIG_FILE}")

    if [[ "${port}" =~ ^[0-9]+$ ]]; then
        FRONTEND_PORT="${port}"
    fi
}

start_backend() {
    echo -e "${GREEN}Starting backend...${NC}"
    cd "${BACKEND_DIR}"

    if [ ! -d "${BACKEND_DIR}/vendor" ] && [ ! -f "${BACKEND_DIR}/go.sum" ]; then
        echo "Downloading Go modules..."
        go mod download
    fi

    go run . -config "${CONFIG_FILE}" &
    BACKEND_PID=$!

    echo "Waiting for backend to start on port 8888..."
    local retries=30
    while [ $retries -gt 0 ]; do
        if curl -s http://127.0.0.1:8888/healthz >/dev/null 2>&1; then
            echo -e "${GREEN}Backend is ready!${NC}"
            return 0
        fi
        sleep 1
        retries=$((retries - 1))
    done

    echo -e "${RED}Backend failed to start within 30 seconds.${NC}"
    return 1
}

start_frontend() {
    echo -e "${GREEN}Starting frontend...${NC}"
    cd "${FRONTEND_DIR}"
    npm run dev -- --host 127.0.0.1 --port "${FRONTEND_PORT}" &
    FRONTEND_PID=$!
    echo -e "${GREEN}Frontend dev server starting on port ${FRONTEND_PORT}...${NC}"
}

main() {
    echo "========================================"
    echo "  CPA-Gateway Development Server Launcher"
    echo "========================================"

    check_deps
    check_go_version
    setup_config
    load_frontend_port
    setup_frontend
    start_backend
    start_frontend

    echo ""
    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN}  All services started successfully!${NC}"
    echo -e "${GREEN}========================================${NC}"
    echo ""
    echo "  Backend:  http://127.0.0.1:8888"
    echo "  Frontend: http://127.0.0.1:${FRONTEND_PORT}"
    echo ""
    echo "  API endpoints:"
    echo "    - Health:  http://127.0.0.1:8888/healthz"
    echo "    - Panel:   http://127.0.0.1:8888/api/panel"
    echo "    - Proxy:   http://127.0.0.1:8888/v1/chat/completions"
    echo ""
    echo "Press Ctrl+C to stop all services."
    echo ""

    wait
}

main "$@"
