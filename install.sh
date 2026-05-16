#!/usr/bin/env bash
# fxfiles-analytics installer
#
# Fresh install OR idempotent update — auto-detected from the presence of
# /etc/fxfiles-analytics/fxfiles-analytics.env.
#
# Architecture decisions (see ADVISORS.md if you want the reasoning):
#  - Postgres runs in a dedicated Docker container `postgres-analytics`
#    mapped to 127.0.0.1:5433 so it can never be reached from the
#    internet (Docker's iptables rules bypass UFW; the bind is the real
#    protection). The pinning-service's existing `postgres-pinning`
#    container is left alone — see Q "shared vs separate Postgres".
#  - Service runs as a dedicated unprivileged user `fxfiles-analytics`
#    under heavy systemd sandboxing.
#  - Layout follows FHS: /opt for binaries, /etc for config, /var/lib
#    for state, /var/backups for backups, /var/log for logs.
#  - Update mode preserves .env, only rebuilds the binary when source
#    changed, never touches the Postgres container or its data.
#
# Run as root.

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

SERVICE_NAME="fxfiles-analytics"
SERVICE_USER="fxfiles-analytics"

INSTALL_DIR="/opt/${SERVICE_NAME}"
BIN_DIR="${INSTALL_DIR}/bin"
CONFIG_DIR="/etc/${SERVICE_NAME}"
CONFIG_FILE="${CONFIG_DIR}/${SERVICE_NAME}.env"
STATE_DIR="/var/lib/${SERVICE_NAME}"
LOG_DIR="/var/log/${SERVICE_NAME}"
BACKUP_DIR="/var/backups/${SERVICE_NAME}"

SYSTEMD_UNIT="/etc/systemd/system/${SERVICE_NAME}.service"
NGINX_AVAILABLE="/etc/nginx/sites-available/${SERVICE_NAME}"
NGINX_ENABLED="/etc/nginx/sites-enabled/${SERVICE_NAME}"

PG_CONTAINER="postgres-analytics"
PG_HOST_PORT="5433"
PG_DB="fxfiles_analytics"
PG_USER="analytics_user"
PG_OWNER="analytics_owner"     # NOLOGIN role that owns the schema
PG_IMAGE="postgres:16-alpine"
PG_VOLUME="postgres-analytics-data"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IS_UPDATE=false

# ---------------------------------------------------------------------------
# Pretty printing
# ---------------------------------------------------------------------------

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
print_info()    { echo -e "${BLUE}[INFO]${NC} $1"; }
print_step()    { echo -e "\n${GREEN}== $1 ==${NC}"; }
print_warn()    { echo -e "${YELLOW}[WARN]${NC} $1"; }
print_error()   { echo -e "${RED}[ERROR]${NC} $1" >&2; }
print_success() { echo -e "${GREEN}[OK]${NC} $1"; }

confirm() {
    # Default-yes prompt. Returns 0 on Y/y/empty, 1 on n/N.
    local prompt="$1"
    local reply
    read -r -p "$prompt [Y/n]: " reply || true
    [[ "$reply" =~ ^[Nn]$ ]] && return 1
    return 0
}

# ---------------------------------------------------------------------------
# Pre-flight checks
# ---------------------------------------------------------------------------

require_root() {
    if [[ $EUID -ne 0 ]]; then
        print_error "Must run as root (use sudo)."
        exit 1
    fi
}

detect_mode() {
    if [[ -f "$CONFIG_FILE" ]]; then
        IS_UPDATE=true
        print_info "Existing installation detected — running in UPDATE mode."
    else
        print_info "No existing installation — running in FRESH INSTALL mode."
    fi
}

preflight() {
    print_step "Pre-flight checks"

    # OS
    if [[ ! -f /etc/os-release ]]; then
        print_error "Cannot detect OS — /etc/os-release missing."
        exit 1
    fi
    # shellcheck disable=SC1091
    source /etc/os-release
    case "$ID" in
        ubuntu|debian)
            print_success "OS: $PRETTY_NAME"
            ;;
        *)
            print_warn "Untested OS: $PRETTY_NAME. Continuing — but YMMV."
            ;;
    esac

    # Disk free (need ~500 MB for build + image + state)
    local free_mb
    free_mb=$(df -Pm /opt 2>/dev/null | awk 'NR==2 {print $4}')
    if [[ -n "$free_mb" && "$free_mb" -lt 1024 ]]; then
        print_warn "/opt has only ${free_mb} MB free — recommend at least 1 GB."
        confirm "Continue anyway?" || exit 1
    fi

    # systemd
    if ! command -v systemctl >/dev/null 2>&1; then
        print_error "systemd not found — this installer targets systemd hosts."
        exit 1
    fi
}

ensure_packages() {
    print_step "Ensuring system packages"
    local need=()
    local pkg
    for pkg in curl ca-certificates gnupg lsb-release ufw nginx \
               dnsutils jq postgresql-client golang-go; do
        if ! dpkg -s "$pkg" >/dev/null 2>&1; then
            need+=("$pkg")
        fi
    done
    if [[ ${#need[@]} -gt 0 ]]; then
        print_info "Installing: ${need[*]}"
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y --no-install-recommends "${need[@]}"
    else
        print_success "All required apt packages already installed."
    fi

    # certbot via snap is cleaner than the apt version on older Debians.
    if ! command -v certbot >/dev/null 2>&1; then
        print_info "Installing certbot..."
        apt-get install -y --no-install-recommends certbot python3-certbot-nginx
    fi

    if ! command -v docker >/dev/null 2>&1; then
        print_warn "Docker not installed. Installing Docker CE..."
        install -m 0755 -d /etc/apt/keyrings
        curl -fsSL https://download.docker.com/linux/${ID}/gpg \
            | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
        chmod a+r /etc/apt/keyrings/docker.gpg
        echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
            https://download.docker.com/linux/${ID} $(. /etc/os-release; echo "$VERSION_CODENAME") stable" \
            > /etc/apt/sources.list.d/docker.list
        apt-get update -qq
        apt-get install -y --no-install-recommends \
            docker-ce docker-ce-cli containerd.io docker-buildx-plugin
        systemctl enable --now docker
    fi
    print_success "Docker: $(docker --version | head -1)"
}

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

prompt_fresh_config() {
    print_step "Configuration (fresh install)"

    read -r -p "Public domain for the analytics endpoint (e.g. analytics.cloud.fx.land): " DOMAIN
    [[ -z "$DOMAIN" ]] && { print_error "Domain required."; exit 1; }

    read -r -p "Email for Let's Encrypt notifications: " LETSENCRYPT_EMAIL
    [[ -z "$LETSENCRYPT_EMAIL" ]] && { print_error "Email required."; exit 1; }

    read -r -p "Listen address for the Go binary [127.0.0.1:8080]: " LISTEN_ADDR
    LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:8080}"

    read -r -p "Allowed gateway suffixes (comma-separated) [.ipfs.dweb.link,.ipfs.cloud.fx.land]: " ALLOWED_GATEWAYS
    ALLOWED_GATEWAYS="${ALLOWED_GATEWAYS:-.ipfs.dweb.link,.ipfs.cloud.fx.land}"

    read -r -p "Postgres host port [${PG_HOST_PORT}]: " pg_port_in
    PG_HOST_PORT="${pg_port_in:-$PG_HOST_PORT}"

    # Generate a random password — never prompt for one and never log it
    # to anything but $CONFIG_FILE (chmod 600).
    PG_PASSWORD="$(openssl rand -base64 32 | tr -d '/+=' | head -c 32)"

    echo ""
    print_info "Summary:"
    echo "  Domain:           $DOMAIN"
    echo "  Listen:           $LISTEN_ADDR"
    echo "  Allowed gateways: $ALLOWED_GATEWAYS"
    echo "  Postgres:         127.0.0.1:${PG_HOST_PORT} (container ${PG_CONTAINER})"
    echo "  Install dir:      $INSTALL_DIR"
    echo "  Config:           $CONFIG_FILE"
    echo ""
    confirm "Proceed with install?" || exit 1
}

prompt_update_config() {
    print_step "Configuration (update)"

    # Backup existing config so we always have a known-good rollback.
    local backup="${CONFIG_FILE}.backup.$(date -u +%Y%m%d%H%M%S)"
    cp -p "$CONFIG_FILE" "$backup"
    chmod 600 "$backup"
    print_info "Backed up current config to $backup"

    # shellcheck disable=SC1090
    source "$CONFIG_FILE"

    # Re-derive variables we use later.
    DOMAIN="$(grep -E '^# DOMAIN=' "$CONFIG_FILE" 2>/dev/null | cut -d= -f2- || echo "")"
    if [[ -z "$DOMAIN" && -f "${CONFIG_DIR}/domain" ]]; then
        DOMAIN="$(cat "${CONFIG_DIR}/domain")"
    fi
    if [[ -z "$DOMAIN" ]]; then
        read -r -p "Public domain (not stored in current config): " DOMAIN
    fi
    LETSENCRYPT_EMAIL="$(grep -E '^# LETSENCRYPT_EMAIL=' "$CONFIG_FILE" 2>/dev/null | cut -d= -f2- || echo "")"

    echo ""
    print_info "Current configuration:"
    echo "  Domain:           ${DOMAIN}"
    echo "  Listen:           ${LISTEN_ADDR:-127.0.0.1:8080}"
    echo "  Allowed gateways: ${ALLOWED_GATEWAYS}"
    echo "  PG_DSN:           ${PG_DSN%%password=*}... (redacted)"
    echo ""

    if confirm "Keep existing configuration?"; then
        print_info "Reusing existing config."
    else
        read -r -p "Public domain [${DOMAIN}]: " new_domain
        DOMAIN="${new_domain:-$DOMAIN}"

        read -r -p "Listen address [${LISTEN_ADDR:-127.0.0.1:8080}]: " new_listen
        LISTEN_ADDR="${new_listen:-${LISTEN_ADDR:-127.0.0.1:8080}}"

        read -r -p "Allowed gateways [${ALLOWED_GATEWAYS}]: " new_gws
        ALLOWED_GATEWAYS="${new_gws:-$ALLOWED_GATEWAYS}"
    fi

    # Detect any new env vars in .env.example that aren't in current .env;
    # prompt for them so a future-added var doesn't get missed.
    if [[ -f "${SCRIPT_DIR}/.env.example" ]]; then
        local var_name var_default
        while IFS='=' read -r var_name var_default; do
            [[ -z "$var_name" || "$var_name" =~ ^# ]] && continue
            if ! grep -q "^${var_name}=" "$CONFIG_FILE"; then
                print_info "New configuration variable detected: $var_name"
                read -r -p "  $var_name [$var_default]: " new_value
                new_value="${new_value:-$var_default}"
                echo "${var_name}=${new_value}" >> "$CONFIG_FILE"
            fi
        done < "${SCRIPT_DIR}/.env.example"
    fi
}

# ---------------------------------------------------------------------------
# Postgres in Docker
# ---------------------------------------------------------------------------

ensure_postgres() {
    print_step "PostgreSQL container"

    if docker inspect "$PG_CONTAINER" >/dev/null 2>&1; then
        print_info "Container ${PG_CONTAINER} already exists."
        # Just make sure it's running. Binding audit already ran in main().
        if ! docker ps --format '{{.Names}}' | grep -q "^${PG_CONTAINER}\$"; then
            print_info "Starting stopped container ${PG_CONTAINER}..."
            docker start "$PG_CONTAINER" >/dev/null
        fi
        return
    fi

    print_info "Creating new postgres container ${PG_CONTAINER} on 127.0.0.1:${PG_HOST_PORT}"

    # Pull the image first so any network/auth issue surfaces here, not
    # mid-`docker run`.
    docker pull "$PG_IMAGE"

    # Generate the initial superuser password locally (used only once,
    # for the initial role creation, then we drop privileges for the app
    # role). We DON'T put it in $CONFIG_FILE — the app role's password
    # lives there instead.
    local pg_root_pw
    pg_root_pw="$(openssl rand -base64 32 | tr -d '/+=' | head -c 32)"

    docker run -d \
        --name "$PG_CONTAINER" \
        --restart unless-stopped \
        -p "127.0.0.1:${PG_HOST_PORT}:5432" \
        -v "${PG_VOLUME}:/var/lib/postgresql/data" \
        -e "POSTGRES_PASSWORD=${pg_root_pw}" \
        -e POSTGRES_DB=postgres \
        --health-cmd='pg_isready -U postgres' \
        --health-interval=10s \
        --health-timeout=5s \
        --health-retries=5 \
        "$PG_IMAGE" >/dev/null

    print_info "Waiting for postgres to accept connections..."
    local i
    for i in {1..30}; do
        if docker exec "$PG_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done
    if ! docker exec "$PG_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
        print_error "Postgres didn't come up after 30s. Container logs:"
        docker logs --tail 40 "$PG_CONTAINER" >&2
        exit 1
    fi

    # Provision roles & database with least-privilege separation. The
    # owner role (NOLOGIN) owns the schema; the runtime role (LOGIN)
    # only gets DML — no DDL, no superuser, no CREATEDB.
    print_info "Provisioning roles and database..."
    docker exec -i -e "PGPASSWORD=${pg_root_pw}" "$PG_CONTAINER" \
        psql -v ON_ERROR_STOP=1 -U postgres -d postgres <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${PG_OWNER}') THEN
    CREATE ROLE ${PG_OWNER} NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${PG_USER}') THEN
    CREATE ROLE ${PG_USER} LOGIN PASSWORD '${PG_PASSWORD}';
  ELSE
    ALTER ROLE ${PG_USER} WITH LOGIN PASSWORD '${PG_PASSWORD}';
  END IF;
END \$\$;

SELECT 'CREATE DATABASE ${PG_DB} OWNER ${PG_OWNER}'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = '${PG_DB}')
\\gexec

REVOKE ALL ON DATABASE ${PG_DB} FROM PUBLIC;
GRANT CONNECT ON DATABASE ${PG_DB} TO ${PG_USER};
SQL

    docker exec -i -e "PGPASSWORD=${pg_root_pw}" "$PG_CONTAINER" \
        psql -v ON_ERROR_STOP=1 -U postgres -d "$PG_DB" <<SQL
REVOKE ALL ON SCHEMA public FROM PUBLIC;
ALTER SCHEMA public OWNER TO ${PG_OWNER};
GRANT USAGE ON SCHEMA public TO ${PG_USER};
ALTER DEFAULT PRIVILEGES FOR ROLE ${PG_OWNER} IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ${PG_USER};
ALTER DEFAULT PRIVILEGES FOR ROLE ${PG_OWNER} IN SCHEMA public
  GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO ${PG_USER};
SQL

    print_success "Postgres container ${PG_CONTAINER} is ready."
}

apply_migrations() {
    print_step "Applying migrations"
    local mig
    if [[ ! -d "${SCRIPT_DIR}/migrations" ]]; then
        print_warn "No migrations/ directory — skipping."
        return
    fi
    for mig in "${SCRIPT_DIR}"/migrations/*.sql; do
        [[ -e "$mig" ]] || { print_info "No SQL files found in migrations/."; return; }
        local name
        name="$(basename "$mig")"
        print_info "Applying $name (as $PG_OWNER via superuser)..."
        # Run as superuser so DDL is executed by the owner; then
        # ALTER OWNER to $PG_OWNER. The runtime app role never gets DDL.
        docker exec -i "$PG_CONTAINER" \
            psql -v ON_ERROR_STOP=1 -U postgres -d "$PG_DB" < "$mig"
    done

    # Re-apply ownership in case the migration created new tables or
    # sequences without explicit OWNER. Belt-and-braces — without the
    # sequence loop, a future migration that adds a SERIAL column would
    # silently fail INSERTs from the runtime role with permission denied
    # on the implicit sequence.
    docker exec -i "$PG_CONTAINER" \
        psql -v ON_ERROR_STOP=1 -U postgres -d "$PG_DB" <<SQL
DO \$\$
DECLARE r RECORD;
BEGIN
  FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' LOOP
    EXECUTE format('ALTER TABLE public.%I OWNER TO ${PG_OWNER}', r.tablename);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.%I TO ${PG_USER}', r.tablename);
  END LOOP;
  FOR r IN SELECT sequence_name FROM information_schema.sequences WHERE sequence_schema = 'public' LOOP
    EXECUTE format('ALTER SEQUENCE public.%I OWNER TO ${PG_OWNER}', r.sequence_name);
    EXECUTE format('GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.%I TO ${PG_USER}', r.sequence_name);
  END LOOP;
END \$\$;
SQL
    print_success "Migrations applied."
}

audit_pg_binding() {
    # Refuse to proceed if anything publishes 5432 or our chosen port to
    # 0.0.0.0. The defence-in-depth UFW rules below help against native
    # processes but Docker writes its own iptables rules in the DOCKER
    # chain that bypass UFW — so the 127.0.0.1 bind is the real wall.
    local pubs
    pubs=$(docker ps --format '{{.Names}} {{.Ports}}' | grep -E '0\.0\.0\.0:(5432|'"$PG_HOST_PORT"')|:::(5432|'"$PG_HOST_PORT"')' || true)
    if [[ -n "$pubs" ]]; then
        print_error "Refusing to continue — these containers publish Postgres to the internet:"
        print_error "$pubs"
        print_error "Rebind them to 127.0.0.1 first (the pinning-service installer has a helper)."
        exit 1
    fi
    print_success "All Postgres containers bound to localhost only."
}

# ---------------------------------------------------------------------------
# Firewall
# ---------------------------------------------------------------------------

setup_firewall() {
    print_step "Firewall (UFW)"

    # If the operator plans to also run the FxFiles pinning-service on
    # this host, we pre-allow the IPFS swarm + cluster comms ports here.
    # The pinning-service's own installer adds DENY rules for the API
    # ports (5001, 9094, 9095) but does NOT add ALLOW rules for libp2p
    # (4001) or cluster comms (9096) — those need to reach the internet
    # for IPFS to peer at all. Asking up-front avoids a "why isn't my
    # IPFS connecting?" debugging session later.
    local pin_coexist=false
    if [[ "$IS_UPDATE" != true ]]; then
        local reply
        read -r -p "Will you also install pinning-service on this host? [y/N]: " reply || true
        [[ "$reply" =~ ^[Yy]$ ]] && pin_coexist=true
    fi

    if ! ufw status 2>/dev/null | grep -q "^Status: active"; then
        print_warn "UFW is INACTIVE."
        if confirm "Enable UFW now with allow 22/80/443 + default-deny incoming?"; then
            # Deliberately NOT `ufw --force reset` — that wipes any
            # rules the operator already added (custom SSH port, VPN
            # subnet, etc.) and is the fast path to locking yourself
            # out. We just apply policy on top.
            ufw default deny incoming >/dev/null
            ufw default allow outgoing >/dev/null
            ufw allow 22/tcp comment 'ssh' >/dev/null
            ufw allow 80/tcp comment 'http' >/dev/null
            ufw allow 443/tcp comment 'https' >/dev/null
            ufw --force enable >/dev/null
            print_success "UFW enabled."
        else
            print_warn "Skipping UFW setup — recommend revisiting before exposing the service."
            return
        fi
    else
        # Already active: just make sure the bare-minimum ports we need
        # are allowed. Don't touch the rest of the rule set.
        ufw allow 80/tcp >/dev/null 2>&1 || true
        ufw allow 443/tcp >/dev/null 2>&1 || true
    fi

    # Pre-allow ports the pinning-service will need so its installer
    # (which doesn't add these itself) runs cleanly later. These ports
    # are harmless if pinning never gets installed — without a service
    # bound to them the kernel rejects connections anyway; UFW allow
    # just makes the eventual install seamless.
    if [[ "$pin_coexist" = true ]]; then
        ufw allow 4001/tcp comment 'ipfs libp2p swarm' >/dev/null 2>&1 || true
        ufw allow 4001/udp comment 'ipfs libp2p swarm (quic)' >/dev/null 2>&1 || true
        ufw allow 9096/tcp comment 'ipfs-cluster peer comms' >/dev/null 2>&1 || true
        print_success "Pre-allowed IPFS ports (4001/tcp, 4001/udp, 9096/tcp) for future pinning-service install."
    fi

    # Defense in depth: deny the Postgres ports even though Docker's
    # iptables rules bypass UFW. The 127.0.0.1 bind set up by both this
    # installer and (later) pinning-service is the real protection;
    # these rules block native processes if anyone later starts a host
    # Postgres on the same port.
    #   5432 → pinning-service's postgres-pinning (when it lands later).
    #   $PG_HOST_PORT (default 5433) → our postgres-analytics.
    if ufw status 2>/dev/null | grep -q "^Status: active"; then
        local port
        for port in 5432 "$PG_HOST_PORT"; do
            if ! ufw status | grep -qE "^${port}/tcp\s+DENY"; then
                ufw deny "${port}/tcp" comment 'postgres — must stay on 127.0.0.1' >/dev/null 2>&1 || true
            fi
        done
        print_success "UFW deny rules in place for 5432 and ${PG_HOST_PORT} (defense in depth)."
    fi
}

# ---------------------------------------------------------------------------
# Build & install
# ---------------------------------------------------------------------------

ensure_service_user() {
    if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
        print_info "Creating service user $SERVICE_USER"
        useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
    fi
}

ensure_dirs() {
    install -d -m 0755 -o root -g root "$INSTALL_DIR" "$BIN_DIR"
    install -d -m 0750 -o root -g "$SERVICE_USER" "$CONFIG_DIR"
    install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_USER" "$STATE_DIR" "$LOG_DIR" "$BACKUP_DIR"
}

build_binary() {
    print_step "Building Go binary"

    local src_hash bin_hash
    src_hash="$(find "$SCRIPT_DIR" -maxdepth 2 -type f \
        \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) \
        -exec sha256sum {} + 2>/dev/null | sort | sha256sum | awk '{print $1}')"

    local hash_file="${INSTALL_DIR}/.src.sha256"
    bin_hash=""
    [[ -f "$hash_file" ]] && bin_hash="$(cat "$hash_file")"

    if [[ "$IS_UPDATE" = true && "$src_hash" = "$bin_hash" && -x "${BIN_DIR}/${SERVICE_NAME}" ]]; then
        print_info "Source hash unchanged — skipping rebuild."
        return
    fi

    # Backup the previous binary so a bad build is rollback-able.
    if [[ -x "${BIN_DIR}/${SERVICE_NAME}" ]]; then
        cp -p "${BIN_DIR}/${SERVICE_NAME}" "${BIN_DIR}/${SERVICE_NAME}.backup"
        print_info "Saved previous binary to ${BIN_DIR}/${SERVICE_NAME}.backup"
    fi

    print_info "Compiling..."
    ( cd "$SCRIPT_DIR" && \
      CGO_ENABLED=0 GOOS=linux \
      go build -trimpath -ldflags="-s -w" -o "${BIN_DIR}/${SERVICE_NAME}.new" ./... )

    mv "${BIN_DIR}/${SERVICE_NAME}.new" "${BIN_DIR}/${SERVICE_NAME}"
    chmod 0755 "${BIN_DIR}/${SERVICE_NAME}"
    echo "$src_hash" > "$hash_file"
    print_success "Built ${BIN_DIR}/${SERVICE_NAME}"
}

write_config_file() {
    print_step "Writing $CONFIG_FILE"

    # On fresh install, write from scratch. On update with "keep existing
    # config", $PG_PASSWORD won't be set so we mustn't rewrite PG_DSN.
    if [[ "$IS_UPDATE" = true && -n "${PG_DSN:-}" && -z "${PG_PASSWORD:-}" ]]; then
        # Update mode keeping existing creds — just rewrite the
        # non-credential fields. Source the existing file, then re-emit.
        # shellcheck disable=SC1090
        source "$CONFIG_FILE"
    else
        PG_DSN="postgres://${PG_USER}:${PG_PASSWORD}@127.0.0.1:${PG_HOST_PORT}/${PG_DB}?sslmode=disable"
    fi

    cat > "$CONFIG_FILE" <<EOF
# fxfiles-analytics configuration — managed by install.sh
# Generated $(date -u +%FT%TZ). Edit by hand if you want, but
# install.sh's update mode will preserve your edits on re-run.

LISTEN_ADDR=${LISTEN_ADDR:-127.0.0.1:8080}
PG_DSN=${PG_DSN}
ALLOWED_GATEWAYS=${ALLOWED_GATEWAYS:-.ipfs.dweb.link,.ipfs.cloud.fx.land}
RATE_LIMIT_PER_MIN=${RATE_LIMIT_PER_MIN:-60}
MAX_DISTINCT_CIDS=${MAX_DISTINCT_CIDS:-100000}
TRUSTED_PROXIES=${TRUSTED_PROXIES:-127.0.0.1/32,::1/128}

# DOMAIN and LETSENCRYPT_EMAIL are not used by the Go binary; they're
# stashed here so update-mode can re-read them.
# DOMAIN=${DOMAIN}
# LETSENCRYPT_EMAIL=${LETSENCRYPT_EMAIL:-}
EOF
    chmod 0640 "$CONFIG_FILE"
    chown root:"$SERVICE_USER" "$CONFIG_FILE"
}

# ---------------------------------------------------------------------------
# systemd
# ---------------------------------------------------------------------------

install_systemd_unit() {
    print_step "Installing systemd unit"

    if [[ ! -f "${SCRIPT_DIR}/deploy/${SERVICE_NAME}.service" ]]; then
        print_error "Missing deploy/${SERVICE_NAME}.service in the source tree."
        exit 1
    fi

    install -m 0644 "${SCRIPT_DIR}/deploy/${SERVICE_NAME}.service" "$SYSTEMD_UNIT"
    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}.service" >/dev/null
    print_success "Systemd unit installed and enabled."
}

start_or_restart_service() {
    print_step "Starting service"
    if systemctl is-active --quiet "${SERVICE_NAME}.service"; then
        systemctl restart "${SERVICE_NAME}.service"
    else
        systemctl start "${SERVICE_NAME}.service"
    fi

    # Give it a beat to fail fast if it's going to.
    sleep 3
    if ! systemctl is-active --quiet "${SERVICE_NAME}.service"; then
        print_error "Service did not start. Recent journal:"
        journalctl -u "${SERVICE_NAME}.service" --no-pager -n 30 >&2
        exit 1
    fi
    print_success "${SERVICE_NAME} is running (PID $(systemctl show -p MainPID --value "${SERVICE_NAME}.service"))."
}

# ---------------------------------------------------------------------------
# Nginx + TLS
# ---------------------------------------------------------------------------

install_nginx_site() {
    print_step "Configuring nginx"
    local tmpl="${SCRIPT_DIR}/deploy/nginx-${SERVICE_NAME}.conf.template"
    if [[ ! -f "$tmpl" ]]; then
        print_error "Missing $tmpl"; exit 1
    fi

    # Strip the Go listen prefix so nginx upstream is "host:port"
    local upstream="${LISTEN_ADDR:-127.0.0.1:8080}"

    sed -e "s|__DOMAIN__|${DOMAIN}|g" \
        -e "s|__UPSTREAM__|${upstream}|g" \
        "$tmpl" > "$NGINX_AVAILABLE"

    ln -sf "$NGINX_AVAILABLE" "$NGINX_ENABLED"
    # Make sure the default site doesn't shadow our server_name.
    rm -f /etc/nginx/sites-enabled/default

    if ! nginx -t; then
        print_error "nginx -t failed — see above."
        exit 1
    fi
    systemctl reload nginx
    print_success "nginx config in place and reloaded."
}

issue_or_renew_cert() {
    print_step "TLS certificate"

    if [[ -d "/etc/letsencrypt/live/${DOMAIN}" ]]; then
        print_info "Certificate already present for ${DOMAIN} — relying on certbot's renewal timer."
        return
    fi

    # Pre-flight: does the domain actually resolve here? If not, certbot
    # will fail anyway — surface it cleanly instead of mid-installer.
    local resolved
    resolved="$(dig +short "$DOMAIN" 2>/dev/null | tail -n1 || true)"
    if [[ -z "$resolved" ]]; then
        print_warn "$DOMAIN does not resolve. Skipping certbot — set DNS, then re-run install.sh."
        return
    fi

    # If the resolved IP doesn't match a local interface, certbot may
    # still succeed (proxy/CDN scenarios) but warn anyway.
    if ! ip -o addr | awk '{print $4}' | cut -d/ -f1 | grep -qx "$resolved"; then
        print_warn "$DOMAIN resolves to $resolved which isn't a local interface. Certbot will try anyway."
    fi

    if certbot --nginx \
        -d "$DOMAIN" \
        --non-interactive \
        --agree-tos \
        --redirect \
        --email "$LETSENCRYPT_EMAIL"; then
        print_success "Certificate issued for ${DOMAIN}."
    else
        print_warn "Certbot failed — service is still running on HTTP. Re-run install.sh once DNS is correct."
    fi
}

# ---------------------------------------------------------------------------
# Smoke test
# ---------------------------------------------------------------------------

smoke_test() {
    print_step "Smoke test"
    local upstream="${LISTEN_ADDR:-127.0.0.1:8080}"
    # /healthz hits the DB, so this proves end-to-end connectivity.
    if curl -fsS --max-time 5 "http://${upstream}/healthz" >/dev/null; then
        print_success "Local healthz OK."
    else
        print_error "Local healthz FAILED — see journalctl -u ${SERVICE_NAME}."
        exit 1
    fi

    if [[ -d "/etc/letsencrypt/live/${DOMAIN}" ]]; then
        if curl -fsS --max-time 10 "https://${DOMAIN}/healthz" >/dev/null; then
            print_success "Public https://${DOMAIN}/healthz OK."
        else
            print_warn "Public healthz failed — DNS, firewall, or nginx may be misconfigured."
        fi
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
    require_root
    detect_mode
    preflight
    ensure_packages

    if [[ "$IS_UPDATE" = true ]]; then
        prompt_update_config
    else
        prompt_fresh_config
    fi

    ensure_service_user
    ensure_dirs

    # Make sure dockerd is actually running. `ensure_packages` enables it
    # on first install, but if Docker was already installed (manually,
    # or from a prior install attempt) the daemon might be stopped — in
    # which case `docker ps` would return empty and silently fool the
    # audit below into thinking no Postgres containers exist.
    systemctl is-active --quiet docker || systemctl start docker

    # Audit BEFORE anything else Postgres-related runs. Catches a
    # pre-existing postgres-pinning (or anything else) that someone
    # bootstrapped on 0.0.0.0:5432 — refuse the install rather than
    # build the rest of the stack on top of an internet-exposed DB.
    audit_pg_binding

    # Postgres + migrations only on fresh install — never re-provision a
    # running container's roles/db, that would clobber credentials.
    if [[ "$IS_UPDATE" = true ]]; then
        apply_migrations
    else
        ensure_postgres
        apply_migrations
    fi

    setup_firewall
    build_binary
    write_config_file
    install_systemd_unit

    # Only configure nginx + TLS on fresh install or when the unit changed.
    # nginx -t catches config drift, certbot's timer handles renewal.
    if [[ "$IS_UPDATE" != true || ! -f "$NGINX_AVAILABLE" ]]; then
        install_nginx_site
        issue_or_renew_cert
    else
        # Re-templating the nginx file is safe and picks up any
        # template changes we shipped.
        install_nginx_site
    fi

    start_or_restart_service
    smoke_test

    echo ""
    print_success "Done. ${SERVICE_NAME} is running and reachable."
    echo "  Local:    http://${LISTEN_ADDR:-127.0.0.1:8080}/healthz"
    [[ -d "/etc/letsencrypt/live/${DOMAIN}" ]] && echo "  Public:   https://${DOMAIN}/healthz"
    echo "  Logs:     journalctl -u ${SERVICE_NAME} -f"
    echo "  Config:   ${CONFIG_FILE}"
    echo "  Backups:  ${BACKUP_DIR}/  (set up a pg_dump cron if you want off-host backups)"
}

main "$@"
