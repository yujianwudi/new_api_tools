#!/usr/bin/env bash
set -euo pipefail

#######################################
# NewAPI Middleware Tool - 一键部署脚本
# 
# 功能:
#   1. 自动检测 NewAPI 容器和数据库配置
#   2. 交互式配置前端密码和 API Key
#   3. 生成 .env 配置文件
#   4. 启动 Docker Compose 服务
#
# 使用方法:
#   ./deploy.sh              # 交互式部署
#   ./deploy.sh --uninstall  # 卸载服务
#   ./deploy.sh --status     # 查看状态
#######################################

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.yml"
COMPOSE_HOST_FILE="${SCRIPT_DIR}/docker-compose.host.yml"
COMPOSE_LOGDB_FILE="${SCRIPT_DIR}/docker-compose.logdb.yml"
SOURCE_SQL_DSN=""

# 由 detect_environment() 设置：host 模式下需要追加 overlay compose 文件
COMPOSE_FILES=("-f" "$COMPOSE_FILE")

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
die() { log_error "$*"; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "缺少必要命令: $1"
}

# 提取 docker compose v2 的语义版本号。
get_docker_compose_v2_version() {
  local output
  output="$(docker compose version --short 2>/dev/null || docker compose version 2>/dev/null || true)"
  if [[ "$output" =~ v?([0-9]+)\.([0-9]+)\.([0-9]+) ]]; then
    printf '%s.%s.%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"
  fi
}

version_at_least() {
  local current="$1" required="$2"
  local current_major current_minor current_patch required_major required_minor required_patch
  IFS=. read -r current_major current_minor current_patch <<< "$current"
  IFS=. read -r required_major required_minor required_patch <<< "$required"

  (( 10#$current_major > 10#$required_major )) ||
    (( 10#$current_major == 10#$required_major && 10#$current_minor > 10#$required_minor )) ||
    (( 10#$current_major == 10#$required_major && 10#$current_minor == 10#$required_minor && 10#$current_patch >= 10#$required_patch ))
}

# docker-compose.host.yml 使用 !reset，只有 Docker Compose v2.24+ 能正确解析。
require_docker_compose_v224() {
  local reason="${1:-当前 Compose 配置}"
  if [[ "${DOCKER_COMPOSE:-}" != "docker compose" ]]; then
    die "${reason} 依赖 !reset 语法，需要 Docker Compose v2.24+；检测到的是旧版 docker-compose，请安装/升级 Compose v2 插件"
  fi

  local version="${DOCKER_COMPOSE_V2_VERSION:-}"
  [[ -n "$version" ]] || version="$(get_docker_compose_v2_version)"
  [[ -n "$version" ]] || die "无法识别 Docker Compose v2 版本；${reason} 需要 v2.24+"
  version_at_least "$version" "2.24.0" || die "Docker Compose v${version} 过旧；${reason} 依赖 !reset 语法，最低需要 v2.24.0"
}

# 优先使用 docker compose v2；仅在没有 v2 时兼容旧 docker-compose。
detect_docker_compose() {
  if docker compose version >/dev/null 2>&1; then
    DOCKER_COMPOSE="docker compose"
    DOCKER_COMPOSE_V2_VERSION="$(get_docker_compose_v2_version)"
  elif command -v docker-compose >/dev/null 2>&1; then
    DOCKER_COMPOSE="docker-compose"
    DOCKER_COMPOSE_V2_VERSION=""
    log_warn "未检测到 Docker Compose v2，暂用旧版 docker-compose；host 网络部署需要 v2.24+"
  else
    die "缺少 Docker Compose；请安装 Docker Compose v2 插件（host 网络部署最低 v2.24）"
  fi
}

trim() { sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'; }

dotenv_quote() {
	local value="${1-}" escaped
	[[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || die "环境变量值不能包含换行"
	escaped="$(printf '%s' "$value" | sed "s/'/\\\\'/g")"
	printf "'%s'" "$escaped"
}

# 生成 32 字符强密码（约 200 bits 熵）
# 字符集：62 字母数字 + 14 个安全特殊符号 = 76 chars
# 刻意排除：$ ` \ " ' = # ; & | < > [ ] / 空格 — 这些会破坏 .env / docker-compose / heredoc 解析
# 注意：- 必须放在 tr 字符类末尾，否则会被解读为范围操作符
generate_strong_password() {
  local alphabet='A-Za-z0-9!@%^*()_+:?,.-'
  local pw=""
  if [[ -r /dev/urandom ]]; then
    pw="$(LC_ALL=C tr -dc "$alphabet" </dev/urandom 2>/dev/null | head -c 32)"
  fi
  if [[ ${#pw} -lt 32 ]] && command -v openssl >/dev/null 2>&1; then
    pw="$(openssl rand 256 2>/dev/null | LC_ALL=C tr -dc "$alphabet" | head -c 32)"
  fi
  if [[ ${#pw} -lt 32 ]]; then
    log_warn "强密码生成失败，回退到 20 字符字母数字"
    pw="$(head -c 256 /dev/urandom 2>/dev/null | LC_ALL=C tr -dc 'A-Za-z0-9' | head -c 20)"
  fi
  echo "$pw"
}

first_csv() {
  echo "${1}" | sed 's/,.*$//'
}

#######################################
# Docker 环境检测函数 (来自 newapi_detect.sh)
#######################################

# DSN 解析只接受可以无歧义拆分的常见形式。所有错误都必须脱敏，禁止输出原始 DSN。
dsn_parse_error() {
  printf '无法安全解析数据库 DSN：%s（原始 DSN 已隐藏）\n' "$1" >&2
  return 1
}

dsn_validate_port() {
  local port="${1:-}"
  [[ -z "$port" ]] && return 0
  if [[ ! "$port" =~ ^[0-9]+$ || ${#port} -gt 5 ]] || (( 10#$port < 1 || 10#$port > 65535 )); then
    dsn_parse_error "端口必须是 1-65535 的数字"
  fi
}

dsn_validate_postgres_url_query() {
  local query="${1:-}" parameter key normalized_key

  [[ -n "$query" ]] || return 0
  while :; do
    parameter="${query%%&*}"
    key="${parameter%%=*}"

    # libpq URL-decodes query parameter names. Restrict names to its normal
    # identifier form so percent-encoding cannot disguise a target override.
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || {
      dsn_parse_error "PostgreSQL URL query 含无效或编码后的键名"
      return 1
    }
    normalized_key="${key,,}"
    case "$normalized_key" in
      host|hostaddr|port)
        dsn_parse_error "PostgreSQL URL query 不能覆盖 host、hostaddr 或 port"
        return 1
        ;;
    esac

    [[ "$query" == *'&'* ]] || break
    query="${query#*&}"
  done
}

dsn_url_component() {
  local dsn="$1" component="$2"
  local scheme rest authority path query="" userinfo="" hostport user="" password="" host="" port="" dbname

  [[ "$dsn" != *$'\n'* && "$dsn" != *$'\r'* && "$dsn" != *[[:space:]]* ]] || {
    dsn_parse_error "URL DSN 不能包含空白或换行"
    return 1
  }

  scheme="${dsn%%://*}"
  case "$scheme" in
    postgres|postgresql|mysql) ;;
    *) dsn_parse_error "不支持的 URL scheme"; return 1 ;;
  esac

  rest="${dsn#*://}"
  [[ "$rest" == */* ]] || { dsn_parse_error "URL DSN 缺少 /dbname"; return 1; }
  authority="${rest%%/*}"
  path="${rest#*/}"
  [[ -n "$authority" && -n "$path" && "$path" != *'#'* ]] || {
    dsn_parse_error "URL DSN 的 authority 或 dbname 无效"
    return 1
  }

  dbname="${path%%\?*}"
  if [[ "$path" == *'?'* ]]; then
    query="${path#*\?}"
    if [[ "$scheme" == "postgres" || "$scheme" == "postgresql" ]]; then
      dsn_validate_postgres_url_query "$query" || return 1
    fi
  fi
  [[ -n "$dbname" && "$dbname" != */* ]] || {
    dsn_parse_error "URL DSN 的 dbname 缺失或包含未转义的斜杠"
    return 1
  }

  hostport="$authority"
  if [[ "$authority" == *@* ]]; then
    userinfo="${authority%@*}"
    hostport="${authority##*@}"
    [[ -n "$userinfo" ]] || { dsn_parse_error "URL DSN 的用户信息为空"; return 1; }
    if [[ "$userinfo" == *:* ]]; then
      user="${userinfo%%:*}"
      password="${userinfo#*:}"
    else
      user="$userinfo"
    fi
    [[ -n "$user" ]] || { dsn_parse_error "URL DSN 的用户名为空"; return 1; }
  fi

  if [[ "$hostport" =~ ^\[([^][]+)\](:([0-9]+))?$ ]]; then
    host="${BASH_REMATCH[1]}"
    port="${BASH_REMATCH[3]:-}"
  elif [[ "$hostport" =~ ^([^:]+)(:([0-9]+))?$ ]]; then
    host="${BASH_REMATCH[1]}"
    port="${BASH_REMATCH[3]:-}"
  else
    dsn_parse_error "URL DSN 的 host:port 无法无歧义解析"
    return 1
  fi
  [[ -n "$host" && "$host" != *[[:space:]@/]* ]] || {
    dsn_parse_error "URL DSN 的 host 无效"
    return 1
  }
  dsn_validate_port "$port" || return 1

  case "$component" in
    validate) ;;
    engine) [[ "$scheme" == "mysql" ]] && printf 'mysql\n' || printf 'postgres\n' ;;
    host) printf '%s\n' "$host" ;;
    user) printf '%s\n' "$user" ;;
    password) printf '%s\n' "$password" ;;
    port) printf '%s\n' "$port" ;;
    dbname) printf '%s\n' "$dbname" ;;
    *) dsn_parse_error "内部请求了未知的 URL DSN 字段"; return 1 ;;
  esac
}

dsn_mysql_go_component() {
  local dsn="$1" component="$2"
  local user password address dbname host="" port=""

  [[ "$dsn" != *$'\n'* && "$dsn" != *$'\r'* && "$dsn" != *[[:space:]]* ]] || {
    dsn_parse_error "MySQL Go DSN 不能包含空白或换行"
    return 1
  }
  if [[ "$dsn" =~ ^([^:/@]+):(.*)@tcp\(([^()]*)\)/([^?]+)(\?.*)?$ ]]; then
    user="${BASH_REMATCH[1]}"
    password="${BASH_REMATCH[2]}"
    address="${BASH_REMATCH[3]}"
    dbname="${BASH_REMATCH[4]}"
  else
    dsn_parse_error "MySQL Go DSN 必须形如 user:password@tcp(host:port)/dbname"
    return 1
  fi

  if [[ "$address" =~ ^\[([^][]+)\]:([0-9]+)$ ]]; then
    host="${BASH_REMATCH[1]}"
    port="${BASH_REMATCH[2]}"
  elif [[ "$address" =~ ^([^:]+):([0-9]+)$ ]]; then
    host="${BASH_REMATCH[1]}"
    port="${BASH_REMATCH[2]}"
  else
    dsn_parse_error "MySQL Go DSN 的 tcp 地址必须是 host:port"
    return 1
  fi
  [[ -n "$host" && "$host" != *[[:space:]@/]* && -n "$dbname" && "$dbname" != */* ]] || {
    dsn_parse_error "MySQL Go DSN 的 host 或 dbname 无效"
    return 1
  }
  dsn_validate_port "$port" || return 1

  case "$component" in
    validate) ;;
    engine) printf 'mysql\n' ;;
    host) printf '%s\n' "$host" ;;
    user) printf '%s\n' "$user" ;;
    password) printf '%s\n' "$password" ;;
    port) printf '%s\n' "$port" ;;
    dbname) printf '%s\n' "$dbname" ;;
    *) dsn_parse_error "内部请求了未知的 MySQL Go DSN 字段"; return 1 ;;
  esac
}

dsn_postgres_keyword_component() {
  local dsn="$1" component="$2"
  local token key value normalized_key
  local host="" port="" user="" password="" dbname=""
  local seen_host=false seen_port=false seen_user=false seen_password=false seen_dbname=false
  local -a tokens=()

  [[ "$dsn" != *$'\n'* && "$dsn" != *$'\r'* ]] || {
    dsn_parse_error "PostgreSQL keyword DSN 不能包含换行"
    return 1
  }
  # libpq 的引号/反斜杠转义需要完整词法分析；这里宁可拒绝，也不能误拆凭证。
  [[ "$dsn" != *"'"* && "$dsn" != *'"'* && "$dsn" != *'\'* ]] || {
    dsn_parse_error "暂不支持带引号或反斜杠转义的 PostgreSQL keyword DSN"
    return 1
  }

  read -r -a tokens <<< "$dsn"
  (( ${#tokens[@]} > 0 )) || { dsn_parse_error "PostgreSQL keyword DSN 为空"; return 1; }
  for token in "${tokens[@]}"; do
    [[ "$token" == *=* ]] || {
      dsn_parse_error "PostgreSQL keyword DSN 含无法归属的空白值"
      return 1
    }
    key="${token%%=*}"
    value="${token#*=}"
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || {
      dsn_parse_error "PostgreSQL keyword DSN 含无效键名"
      return 1
    }
    normalized_key="${key,,}"
    case "$normalized_key" in
      host)
        [[ "$seen_host" == false ]] || { dsn_parse_error "PostgreSQL keyword DSN 重复 host"; return 1; }
        seen_host=true; host="$value"
        ;;
      hostaddr)
        dsn_parse_error "PostgreSQL keyword DSN 不支持 hostaddr；它可能绕过 host 改写"
        return 1
        ;;
      port)
        [[ "$seen_port" == false ]] || { dsn_parse_error "PostgreSQL keyword DSN 重复 port"; return 1; }
        seen_port=true; port="$value"
        ;;
      user)
        [[ "$seen_user" == false ]] || { dsn_parse_error "PostgreSQL keyword DSN 重复 user"; return 1; }
        seen_user=true; user="$value"
        ;;
      password)
        [[ "$seen_password" == false ]] || { dsn_parse_error "PostgreSQL keyword DSN 重复 password"; return 1; }
        seen_password=true; password="$value"
        ;;
      dbname)
        [[ "$seen_dbname" == false ]] || { dsn_parse_error "PostgreSQL keyword DSN 重复 dbname"; return 1; }
        seen_dbname=true; dbname="$value"
        ;;
    esac
  done

  [[ "$seen_host" == true && -n "$host" && "$host" != *[[:space:]@/]* ]] || {
    dsn_parse_error "PostgreSQL keyword DSN 缺少有效 host"
    return 1
  }
  dsn_validate_port "$port" || return 1

  case "$component" in
    validate) ;;
    engine) printf 'postgres\n' ;;
    host) printf '%s\n' "$host" ;;
    user) printf '%s\n' "$user" ;;
    password) printf '%s\n' "$password" ;;
    port) printf '%s\n' "$port" ;;
    dbname) printf '%s\n' "$dbname" ;;
    *) dsn_parse_error "内部请求了未知的 PostgreSQL keyword DSN 字段"; return 1 ;;
  esac
}

detect_dsn_format() {
  local dsn="${1:-}"
  [[ -n "$dsn" ]] || return 0
  if [[ "$dsn" =~ ^postgres(ql)?:// ]]; then
    dsn_url_component "$dsn" validate || return 1
    printf 'postgres_url\n'
  elif [[ "$dsn" =~ ^mysql:// ]]; then
    dsn_url_component "$dsn" validate || return 1
    printf 'mysql_url\n'
  elif [[ "$dsn" == *@tcp\(* ]]; then
    dsn_mysql_go_component "$dsn" validate || return 1
    printf 'mysql_go\n'
  elif [[ "$dsn" == *=* ]]; then
    dsn_postgres_keyword_component "$dsn" validate || return 1
    printf 'postgres_keyword\n'
  else
    dsn_parse_error "仅支持 PostgreSQL/MySQL URL、MySQL Go DSN 和 PostgreSQL keyword DSN"
  fi
}

extract_dsn_component() {
  local dsn="${1:-}" component="$2" format
  [[ -n "$dsn" ]] || return 0
  format="$(detect_dsn_format "$dsn")" || return 1
  case "$format" in
    postgres_url|mysql_url) dsn_url_component "$dsn" "$component" ;;
    mysql_go) dsn_mysql_go_component "$dsn" "$component" ;;
    postgres_keyword) dsn_postgres_keyword_component "$dsn" "$component" ;;
    *) dsn_parse_error "内部识别到了未知 DSN 格式"; return 1 ;;
  esac
}

extract_dsn_engine() { extract_dsn_component "${1:-}" engine; }
extract_dsn_host() { extract_dsn_component "${1:-}" host; }
extract_dsn_user() { extract_dsn_component "${1:-}" user; }
extract_dsn_password() { extract_dsn_component "${1:-}" password; }
extract_dsn_port() { extract_dsn_component "${1:-}" port; }
extract_dsn_dbname() { extract_dsn_component "${1:-}" dbname; }

dsn_host_port() {
  local host="$1" port="$2"
  [[ -n "$host" && "$host" != *[[:space:]@/]* ]] || {
    dsn_parse_error "改写后的数据库 host 无效"
    return 1
  }
  dsn_validate_port "$port" || return 1
  if [[ "$host" == *:* ]]; then
    printf '[%s]:%s' "$host" "$port"
  else
    printf '%s:%s' "$host" "$port"
  fi
}

# Preserve the original DSN representation, escaping and connection options;
# only the network host and port are replaced.
rewrite_dsn_host_port() {
  local dsn="$1" new_host="$2" new_port="$3" format address
  format="$(detect_dsn_format "$dsn")" || return 1
  address="$(dsn_host_port "$new_host" "$new_port")" || return 1

  case "$format" in
    postgres_url|mysql_url)
      local scheme rest authority path userinfo_prefix=""
      scheme="${dsn%%://*}"
      rest="${dsn#*://}"
      authority="${rest%%/*}"
      path="${rest#*/}"
      if [[ "$authority" == *@* ]]; then
        userinfo_prefix="${authority%@*}@"
      fi
      printf '%s://%s%s/%s\n' "$scheme" "$userinfo_prefix" "$address" "$path"
      ;;
    mysql_go)
      if [[ "$dsn" =~ ^(.+)@tcp\([^()]*\)/(.*)$ ]]; then
        printf '%s@tcp(%s)/%s\n' "${BASH_REMATCH[1]}" "$address" "${BASH_REMATCH[2]}"
      else
        dsn_parse_error "MySQL Go DSN 无法安全改写地址"
        return 1
      fi
      ;;
    postgres_keyword)
      local token key normalized_key seen_port=false i
      local -a tokens=() rewritten=()
      read -r -a tokens <<< "$dsn"
      for token in "${tokens[@]}"; do
        key="${token%%=*}"
        normalized_key="${key,,}"
        case "$normalized_key" in
          host) rewritten+=("${key}=${new_host}") ;;
          port) rewritten+=("${key}=${new_port}"); seen_port=true ;;
          *) rewritten+=("$token") ;;
        esac
      done
      [[ "$seen_port" == true ]] || rewritten+=("port=${new_port}")
      printf '%s' "${rewritten[0]}"
      for ((i = 1; i < ${#rewritten[@]}; i++)); do
        printf ' %s' "${rewritten[$i]}"
      done
      printf '\n'
      ;;
    *)
      dsn_parse_error "未知 DSN 格式"
      return 1
      ;;
  esac
}

detect_newapi_container() {
  local found=""
  # 按容器名匹配：new-api / new-api-master / new-api-my ...（不含 newapi-tools）
  found="$(docker ps --format '{{.Names}}' | awk 'tolower($0) ~ /(^|[-_])new-api([-_]|$)/ {print; exit}')"
  if [[ -n "$found" ]]; then echo "$found"; return 0; fi

  found="$(docker ps -q --filter 'label=com.docker.compose.service=new-api' | head -n 1 || true)"
  if [[ -n "$found" ]]; then echo "$found"; return 0; fi

  # 按镜像名匹配：允许 fork 后缀（new-api-my:latest 也能命中）
  found="$(docker ps --format '{{.ID}}\t{{.Image}}' | awk 'tolower($2) ~ /(^|\/)new-api([-_:]|$)/ {print $1; exit}')"
  if [[ -n "$found" ]]; then echo "$found"; return 0; fi

  return 1
}

docker_inspect_label() {
  local container="$1" key="$2"
  docker inspect -f "{{ index .Config.Labels \"$key\" }}" "$container" 2>/dev/null || true
}

docker_inspect_env_value() {
  local container="$1" var_name="$2"
  docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$container" 2>/dev/null |
    awk -v k="$var_name" 'index($0, k "=")==1 {print substr($0, length(k)+2); exit}'
}

# Direct database mutations are only safe when NewAPI explicitly declares an
# empty REDIS_CONN_STRING. A missing variable or an inspect failure is treated
# as unknown and therefore remains fail-closed.
detect_newapi_redis_mutation_safety() {
  NEWAPI_REDIS_DISABLED=false

  local env_lines redis_entry redis_value
  if ! env_lines="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$NEWAPI_CONTAINER" 2>/dev/null)"; then
    log_warn "无法读取 NewAPI 容器环境变量；NEWAPI_REDIS_DISABLED=false"
    log_warn "用户、Token、分组和 IP 记录等直接写库操作将被阻止，请改用 NewAPI 管理 API"
    return 0
  fi

  redis_entry="$(printf '%s\n' "$env_lines" | awk -F= '$1=="REDIS_CONN_STRING"{print; exit}')"
  if [[ -z "$redis_entry" ]]; then
    log_warn "NewAPI 未显式声明 REDIS_CONN_STRING，Redis 状态未知；NEWAPI_REDIS_DISABLED=false"
    log_warn "用户、Token、分组和 IP 记录等直接写库操作将被阻止，请改用 NewAPI 管理 API"
    return 0
  fi

  redis_value="${redis_entry#*=}"
  if [[ -z "$redis_value" ]]; then
    NEWAPI_REDIS_DISABLED=true
    log_success "NewAPI 明确配置 REDIS_CONN_STRING=（空），允许受保护的直接数据库写操作"
    return 0
  fi

  log_warn "检测到 NewAPI 已配置 Redis；NEWAPI_REDIS_DISABLED=false"
  log_warn "为避免缓存中的用户/Token 权限延迟失效，相关直接写库操作将被阻止，请改用 NewAPI 管理 API"
}

detect_networks_for_container() {
  local container="$1"
  docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{println $k}}{{end}}' "$container" 2>/dev/null || true
}

container_is_on_network() {
  local container="$1" network="$2"
  docker inspect -f "{{ if (index .NetworkSettings.Networks \"$network\") }}yes{{ end }}" "$container" 2>/dev/null | grep -q '^yes$'
}

detect_db_container_by_compose_service() {
  local project="$1" service="$2"
  docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$service" | head -n 1 || true
}

detect_db_container_by_exposed_port() {
  local network="$1" port_tcp="$2"
  local cid
  while IFS= read -r cid; do
    [[ -z "$cid" ]] && continue
    if docker inspect -f '{{json .Config.ExposedPorts}}' "$cid" 2>/dev/null | grep -q "\"$port_tcp\""; then
      echo "$cid"
      return 0
    fi
  done < <(docker ps -q --filter "network=$network" || true)
  return 0
}

# 获取容器在指定网络上的 IPv4 地址
get_container_ipv4() {
  local container="$1" network="$2"
  docker inspect -f "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$container" 2>/dev/null || true
}

# 判断一个字符串是否是 IPv4 字面量
is_ipv4_literal() {
  [[ "${1:-}" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]
}

# 给定一个 IPv4，在所有运行中的容器里反查“谁在某个 docker 网络上持有这个 IP”。
# 命中则输出 "<网络名>\t<容器名>"（取第一个匹配），否则无输出。
# 用于：NewAPI 是 host 模式、但数据库其实是另一条 bridge 网络上的容器（如 1Panel 托管的 PG），
#       DSN 里硬编码了该容器的 bridge IP（172.x.x.x）——此时 newapi-tools 需要挂进那条网络。
find_container_by_network_ip() {
  local target_ip="${1:-}"
  [[ -z "$target_ip" ]] && return 0
  local cid name net ip
  while IFS= read -r cid; do
    [[ -z "$cid" ]] && continue
    name="$(docker inspect -f '{{.Name}}' "$cid" 2>/dev/null | sed 's#^/##')"
    while IFS=$'\t' read -r net ip; do
      [[ -z "$net" ]] && continue
      if [[ "$ip" == "$target_ip" ]]; then
        printf '%s\t%s\n' "$net" "$name"
        return 0
      fi
    done < <(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{"\t"}}{{$v.IPAddress}}{{"\n"}}{{end}}' "$cid" 2>/dev/null)
  done < <(docker ps -q)
  return 0
}

# 给定一个宿主机端口，找出"把该端口发布到宿主机(0.0.0.0 / 127.0.0.1 / [::])"的容器，
# 并返回它所在的【用户自定义】网络、容器名、以及容器内端口："<网络名>\t<容器名>\t<容器内端口>"。
# 用于：NewAPI 是 host 模式、DSN host 写的是 127.0.0.1，但数据库其实是个容器、
#       端口只发布在宿主机回环上（1Panel / 宝塔默认就这么发布）。这种情况下
#       host.docker.internal(网关 IP) 连不通（docker-proxy 只绑回环），必须把
#       newapi-tools 挂进该容器的网络、用容器名直连。
# 注意：返回的是容器内端口（PortBindings 的 key 侧），而非宿主机映射端口——
#       例如 127.0.0.1:5433->5432 时，同网络直连要用 5432 而不是 5433。
# 排除默认 bridge / host / none：它们不支持按容器名做 DNS 解析。
find_container_by_published_port() {
  local requested_host="${1:-}" target_port="${2:-}"
  [[ -z "$target_port" ]] && return 0
  local cid name net selected_net cport binding_port host_ip host_port candidate match=""
  while IFS= read -r cid; do
    [[ -z "$cid" ]] && continue
    cport=""
    while IFS='|' read -r binding_port host_ip host_port; do
      [[ "$host_port" == "$target_port" ]] || continue
      case "$requested_host" in
        127.0.0.1) [[ "$host_ip" == "127.0.0.1" ]] || continue ;;
        ::1) [[ "$host_ip" == "::1" ]] || continue ;;
        localhost) [[ "$host_ip" == "127.0.0.1" || "$host_ip" == "::1" ]] || continue ;;
        *) continue ;;
      esac
      cport="${binding_port%/*}"
      break
    done < <(docker inspect -f '{{range $p,$bindings := .NetworkSettings.Ports}}{{range $bindings}}{{printf "%s|%s|%s\n" $p .HostIp .HostPort}}{{end}}{{end}}' "$cid" 2>/dev/null)
    [[ -z "$cport" ]] && continue
    name="$(docker inspect -f '{{.Name}}' "$cid" 2>/dev/null | sed 's#^/##')"
    selected_net=""
    while IFS= read -r net; do
      [[ -z "$net" ]] && continue
      case "$net" in bridge|host|none) continue ;; esac
      selected_net="$net"
      break
    done < <(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{println $k}}{{end}}' "$cid" 2>/dev/null)
    [[ -n "$selected_net" ]] || continue
    candidate="${selected_net}"$'\t'"${name}"$'\t'"${cport}"
    if [[ -n "$match" && "$candidate" != "$match" ]]; then
      return 0
    fi
    match="$candidate"
  done < <(docker ps -q)
  [[ -n "$match" ]] && printf '%s\n' "$match"
  return 0
}

#######################################
# 检测 NewAPI 环境
#######################################
detect_environment() {
  log_info "正在检测 NewAPI 环境..."

  # 检测 NewAPI 容器
  NEWAPI_CONTAINER="${NEWAPI_CONTAINER:-}"
  if [[ -z "$NEWAPI_CONTAINER" ]]; then
    NEWAPI_CONTAINER="$(detect_newapi_container)" || die "找不到运行中的 NewAPI 容器（可设置环境变量 NEWAPI_CONTAINER=<容器名或ID> 手动指定）"
  fi
  log_success "找到 NewAPI 容器: $NEWAPI_CONTAINER"

  detect_newapi_redis_mutation_safety

  # 获取 compose 项目信息
  local compose_project compose_files user_compose_file
  compose_project="$(docker_inspect_label "$NEWAPI_CONTAINER" 'com.docker.compose.project' | trim)"
  compose_files="$(docker_inspect_label "$NEWAPI_CONTAINER" 'com.docker.compose.project.config_files' | trim)"

  user_compose_file="${COMPOSE_FILE_OVERRIDE:-}"
  if [[ -z "$user_compose_file" && -n "$compose_files" ]]; then
    user_compose_file="$(first_csv "$compose_files" | trim)"
  fi
  if [[ -n "$user_compose_file" && ! -r "$user_compose_file" ]]; then
    user_compose_file=""
  fi

  # 检测网络
  local networks network_mode
  networks="$(detect_networks_for_container "$NEWAPI_CONTAINER" | trim || true)"
  network_mode="$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$NEWAPI_CONTAINER" 2>/dev/null | trim || true)"

  ORIGINAL_NETWORK=""
  USE_BRIDGE_MODE=false
  USE_HOST_MODE=false

  if [[ "$network_mode" == "host" ]]; then
    # ===== Host 网络模式 =====
    # 注意：NewAPI 用 host 网络，不代表数据库也在宿主机上。
    # 常见反例：数据库是 1Panel / 另一套 compose 托管的容器，挂在某条 bridge 网络上，
    # DSN 里写的是它的 bridge IP（172.x）。这种 IP 只有 host 网络的 NewAPI 碰得到，
    # bridge 里的 newapi-tools 跨不过去。真正的网络方案要等解析完 DSN host 再定（见下方）。
    log_warn "检测到 NewAPI 使用 host 网络模式"
    USE_HOST_MODE=true
    NEWAPI_NETWORK=""
    ORIGINAL_NETWORK="host"
  else
    NEWAPI_NETWORK="${NEWAPI_NETWORK:-}"
    if [[ -z "$NEWAPI_NETWORK" ]]; then
      NEWAPI_NETWORK="$(echo "$networks" | head -n 1 | trim)"
    fi
    [[ -n "$NEWAPI_NETWORK" ]] || die "无法确定 NewAPI 容器的 Docker 网络"
    container_is_on_network "$NEWAPI_CONTAINER" "$NEWAPI_NETWORK" || die "容器 '$NEWAPI_CONTAINER' 未连接到网络 '$NEWAPI_NETWORK'"

    ORIGINAL_NETWORK="$NEWAPI_NETWORK"

    if [[ "$NEWAPI_NETWORK" == "bridge" ]]; then
      log_warn "检测到 NewAPI 使用默认 bridge 网络"
      log_warn "默认 bridge 网络不支持 Docker 服务发现，将使用 IPv4 地址模式"
      log_info ""
      log_info "提示：为获得更好的体验，建议将 NewAPI 部署在用户自定义网络中"
      log_info ""
      USE_BRIDGE_MODE=true

      # 创建一个用户自定义网络供 docker-compose 使用
      # 这样可以避免 "network-scoped alias" 错误
      if ! docker network inspect newapi-tools-network >/dev/null 2>&1; then
        log_info "创建网络 'newapi-tools-network' 供服务使用..."
        docker network create newapi-tools-network || die "创建网络失败"
      fi
      # 使用新创建的网络作为 NEWAPI_NETWORK（供 docker-compose.yml 使用）
      NEWAPI_NETWORK="newapi-tools-network"
      log_success "使用网络: $NEWAPI_NETWORK (数据库连接将使用 IPv4 地址)"
    else
      log_success "检测到网络: $NEWAPI_NETWORK"
    fi
  fi

  # 检测数据库 DSN
  local detected_dsn=""
  detected_dsn="$(docker_inspect_env_value "$NEWAPI_CONTAINER" 'SQL_DSN' || true)"
  [[ -z "$detected_dsn" ]] && detected_dsn="$(docker_inspect_env_value "$NEWAPI_CONTAINER" 'DATABASE_URL' || true)"
  [[ -z "$detected_dsn" ]] && detected_dsn="$(docker_inspect_env_value "$NEWAPI_CONTAINER" 'DB_DSN' || true)"
  SOURCE_SQL_DSN="$detected_dsn"

  DB_ENGINE=""
  DB_DNS=""
  if [[ -n "$detected_dsn" ]]; then
    DB_ENGINE="$(extract_dsn_engine "$detected_dsn")" ||
      die "NewAPI 数据库 DSN 不受支持或格式不安全；原始 DSN 已隐藏"
    DB_DNS="$(extract_dsn_host "$detected_dsn")" ||
      die "无法从 NewAPI 数据库 DSN 安全解析 host；原始 DSN 已隐藏"
  fi

  # ===== Host 模式：完全从 DSN 解析凭证，跳过数据库容器探测 =====
  if [[ "$USE_HOST_MODE" == "true" ]]; then
    [[ -n "$detected_dsn" ]] || die "host 模式下必须能从 NewAPI 容器读取 SQL_DSN，但未检测到"
    [[ -n "$DB_ENGINE" ]] || die "无法从 DSN 识别数据库引擎；原始 DSN 已隐藏"

    # 先解析出完整凭证（下面判断"怎么连"时要用到端口）
    DB_USER="$(extract_dsn_user "$detected_dsn")" || die "无法安全解析数据库用户名；原始 DSN 已隐藏"
    DB_PASSWORD="$(extract_dsn_password "$detected_dsn")" || die "无法安全解析数据库密码；原始 DSN 已隐藏"
    DB_PORT="$(extract_dsn_port "$detected_dsn")" || die "无法安全解析数据库端口；原始 DSN 已隐藏"
    DB_NAME="$(extract_dsn_dbname "$detected_dsn")" || die "无法安全解析数据库名；原始 DSN 已隐藏"

    if [[ "$DB_ENGINE" == "postgres" ]]; then
      DB_PORT="${DB_PORT:-5432}"
      DB_USER="${DB_USER:-postgres}"
      DB_NAME="${DB_NAME:-new-api}"
    elif [[ "$DB_ENGINE" == "mysql" ]]; then
      DB_PORT="${DB_PORT:-3306}"
      DB_USER="${DB_USER:-root}"
      DB_NAME="${DB_NAME:-new-api}"
    fi

    # ===== 决定 newapi-tools 怎么连到这个数据库 =====
    # 四种情形：
    #  (a1) DSN host 是 127.0.0.1/localhost，但数据库其实是个容器、端口只发布在宿主机
    #       回环上（127.0.0.1:PORT，1Panel/宝塔最常见）→ host.docker.internal(网关)连不通，
    #       把 newapi-tools 挂进该容器的网络、用容器名直连，端口改用容器内端口。
    #  (a2) DSN host 是 127.0.0.1/localhost，数据库是宿主机裸装进程(或发布到 0.0.0.0)
    #       → 用 host.docker.internal 走宿主机网关。
    #  (b)  DSN host 是某条 bridge 网络上某容器的 IP → 挂进那条网络，host 改成容器名。
    #  (c)  其它（外部 DB、真实 LAN IP、域名）→ 原样保留，靠 extra_hosts / 宿主路由可达。
    if [[ "$DB_DNS" == "127.0.0.1" || "$DB_DNS" == "localhost" || "$DB_DNS" == "::1" ]]; then
      local _hit _hit_net _hit_name _hit_port
      _hit="$(find_container_by_published_port "$DB_DNS" "$DB_PORT")"
      _hit_net="$(printf '%s' "$_hit" | cut -f1)"
      _hit_name="$(printf '%s' "$_hit" | cut -f2)"
      _hit_port="$(printf '%s' "$_hit" | cut -f3)"
      if [[ -n "$_hit_net" && -n "$_hit_name" ]]; then
        # 情形 (a1)：DB 是容器，端口仅发布在宿主机回环 → 挂进它的网络、用容器名连
        NEWAPI_NETWORK="$_hit_net"
        log_warn "数据库 127.0.0.1:${DB_PORT} 实为容器 '${_hit_name}'（端口仅发布在宿主机回环，网关不可达）"
        log_info "将把 newapi-tools 接入网络 '${_hit_net}'，用容器名 '${_hit_name}:${_hit_port}' 直连（绕开回环端口映射）"
        DB_DNS="$_hit_name"
        DB_PORT="$_hit_port"
        USE_HOST_MODE=false   # 不再脱离 external 网络：我们要挂进它，而非走 host overlay
        ORIGINAL_NETWORK="$_hit_net"
      else
        # 情形 (a2)：DB 在宿主机回环但非容器（或发布到 0.0.0.0）→ 走宿主机网关
        DB_DNS="host.docker.internal"
        log_info "数据库地址改写: 127.0.0.1 → host.docker.internal（数据库在宿主机回环上）"
      fi
    elif is_ipv4_literal "$DB_DNS"; then
      local _hit _hit_net _hit_name
      _hit="$(find_container_by_network_ip "$DB_DNS")"
      _hit_net="$(printf '%s' "$_hit" | cut -f1)"
      _hit_name="$(printf '%s' "$_hit" | cut -f2)"
      if [[ -n "$_hit_net" && -n "$_hit_name" ]]; then
        NEWAPI_NETWORK="$_hit_net"
        log_warn "数据库 ${DB_DNS} 是容器 '${_hit_name}' 在 bridge 网络 '${_hit_net}' 上的 IP"
        log_info "将把 newapi-tools 接入网络 '${_hit_net}'，并用容器名连接（IP 重启会变，容器名不会）"
        DB_DNS="$_hit_name"
        USE_HOST_MODE=false   # 不再走 host overlay：我们要挂进 external 网络，而非脱离它
        ORIGINAL_NETWORK="$_hit_net"
      else
        log_warn "数据库地址 ${DB_DNS} 是 IP 但不属于任何已知 docker 网络容器，按外部地址原样使用"
      fi
    else
      log_info "数据库地址为主机名/外部地址，原样使用: ${DB_DNS}"
    fi

    if [[ "$USE_HOST_MODE" == "true" ]]; then
      # 情形 (a)/(c)：数据库走宿主机（host.docker.internal）或外部地址，
      # newapi-tools 无需任何 external 网络 → 加载 host overlay 去掉 newapi-network 依赖。
      if [[ -f "$COMPOSE_HOST_FILE" ]]; then
        require_docker_compose_v224 "host 网络叠加层 ${COMPOSE_HOST_FILE}"
        COMPOSE_FILES=("-f" "$COMPOSE_FILE" "-f" "$COMPOSE_HOST_FILE")
      else
        log_warn "未找到 $COMPOSE_HOST_FILE，host 模式可能启动失败"
      fi
      log_success "检测到数据库 (host 模式): $DB_ENGINE @ $DB_DNS:$DB_PORT/$DB_NAME"
    else
      # 情形 (b)：数据库在另一条 bridge 网络上，newapi-tools 已改为挂进该网络（NEWAPI_NETWORK）。
      # 用主 compose（newapi-network=external 指向该网络），不加载 host overlay。
      log_success "检测到数据库 (跨网 bridge): $DB_ENGINE @ $DB_DNS:$DB_PORT/$DB_NAME (网络: $NEWAPI_NETWORK)"
    fi
    return 0
  fi

  # 检测数据库容器
  local db_container="" db_service=""
  if [[ -n "$compose_project" ]]; then
    local pg_cid mysql_cid
    pg_cid="$(detect_db_container_by_compose_service "$compose_project" 'postgres')"
    mysql_cid="$(detect_db_container_by_compose_service "$compose_project" 'mysql')"
    if [[ -n "$pg_cid" && -z "$mysql_cid" ]]; then
      DB_ENGINE="${DB_ENGINE:-postgres}"
      db_container="$pg_cid"
      db_service="postgres"
    elif [[ -n "$mysql_cid" && -z "$pg_cid" ]]; then
      DB_ENGINE="${DB_ENGINE:-mysql}"
      db_container="$mysql_cid"
      db_service="mysql"
    fi
  fi

  # 通过端口检测（使用原始网络，因为数据库可能还未连接到新网络）
  local detect_network="${ORIGINAL_NETWORK:-$NEWAPI_NETWORK}"
  if [[ -z "$db_container" ]]; then
    local pg_cid mysql_cid
    pg_cid="$(detect_db_container_by_exposed_port "$detect_network" '5432/tcp' || true)"
    mysql_cid="$(detect_db_container_by_exposed_port "$detect_network" '3306/tcp' || true)"
    if [[ -n "$pg_cid" && -z "$mysql_cid" ]]; then
      DB_ENGINE="${DB_ENGINE:-postgres}"
      db_container="$pg_cid"
    elif [[ -n "$mysql_cid" && -z "$pg_cid" ]]; then
      DB_ENGINE="${DB_ENGINE:-mysql}"
      db_container="$mysql_cid"
    fi
  fi

  DB_ENGINE="${DB_ENGINE:-postgres}"

  # 尝试常见容器名（使用原始网络）
  if [[ -z "$db_container" ]]; then
    if docker ps -q --filter "network=$detect_network" --filter "name=^/postgres$" | head -n 1 | grep -q .; then
      db_container="postgres"
      DB_ENGINE="postgres"
      db_service="postgres"
    elif docker ps -q --filter "network=$detect_network" --filter "name=^/mysql$" | head -n 1 | grep -q .; then
      db_container="mysql"
      DB_ENGINE="mysql"
      db_service="mysql"
    fi
  fi

  [[ -n "$db_container" ]] || die "在网络 '$detect_network' 上找不到数据库容器"
  DB_CONTAINER="$db_container"

  # 设置 DB_DNS
  # 优先级：1. 用户指定的 DB_DNS  2. IPv4 地址（bridge模式必须）  3. 服务名
  if [[ -n "$DB_DNS" ]]; then
    # 用户已指定（从 SQL_DSN 解析出来），保持不变
    log_info "使用指定的数据库主机: $DB_DNS"
  else
    # 尝试获取 IPv4 地址
    local db_ipv4=""
    db_ipv4="$(get_container_ipv4 "$db_container" "$detect_network" | trim)"

    if [[ "$USE_BRIDGE_MODE" == "true" ]]; then
      # Bridge 模式：必须使用 IPv4 地址，因为不支持服务发现
      if [[ -n "$db_ipv4" ]]; then
        DB_DNS="$db_ipv4"
        log_info "使用数据库 IPv4 地址: $db_ipv4 (bridge 模式)"
      else
        die "无法获取数据库容器的 IPv4 地址，请手动指定 DB_DNS 环境变量"
      fi
    else
      # 用户自定义网络：优先使用 IPv4，其次使用服务名
      if [[ -n "$db_ipv4" ]]; then
        DB_DNS="$db_ipv4"
        log_info "使用数据库 IPv4 地址: $db_ipv4"
      elif [[ -n "$db_service" ]]; then
        DB_DNS="$db_service"
        log_info "使用数据库服务名: $db_service"
      else
        db_service="$(docker_inspect_label "$db_container" 'com.docker.compose.service' | trim)"
        if [[ -n "$db_service" ]]; then
          DB_DNS="$db_service"
        else
          DB_DNS="$db_container"
        fi
        log_info "使用数据库主机: $DB_DNS"
      fi
    fi
  fi

  # 获取数据库凭证
  if [[ "$DB_ENGINE" == "postgres" ]]; then
    DB_PORT="5432"
    DB_USER="$(docker_inspect_env_value "$db_container" 'POSTGRES_USER' || true)"
    DB_NAME="$(docker_inspect_env_value "$db_container" 'POSTGRES_DB' || true)"
    DB_PASSWORD="$(docker_inspect_env_value "$db_container" 'POSTGRES_PASSWORD' || true)"
    DB_USER="${DB_USER:-postgres}"
    DB_NAME="${DB_NAME:-new-api}"
  elif [[ "$DB_ENGINE" == "mysql" ]]; then
    DB_PORT="3306"
    DB_USER="$(docker_inspect_env_value "$db_container" 'MYSQL_USER' || true)"
    DB_NAME="$(docker_inspect_env_value "$db_container" 'MYSQL_DATABASE' || true)"
    DB_PASSWORD="$(docker_inspect_env_value "$db_container" 'MYSQL_PASSWORD' || true)"
    [[ -z "$DB_PASSWORD" ]] && DB_PASSWORD="$(docker_inspect_env_value "$db_container" 'MYSQL_ROOT_PASSWORD' || true)"
    DB_USER="${DB_USER:-root}"
    DB_NAME="${DB_NAME:-new-api}"
  else
    die "不支持的数据库引擎: $DB_ENGINE"
  fi

  log_success "检测到数据库: $DB_ENGINE @ $DB_DNS:$DB_PORT/$DB_NAME"
}

#######################################
# 检测日志分库（LOG_SQL_DSN）
#
# 部分 NewAPI fork 用 LOG_SQL_DSN 把 logs 表整张分离到独立库。本工具需读取该库
# 才能看到实时日志/流量。本函数从 NewAPI 容器读取 LOG_SQL_DSN，解析并做与主库
# 同款的「容器名/网络」改写，产出：
#   LOG_SQL_DSN_FINAL  写入工具 .env 的最终 DSN（容器名直连/host.docker.internal）
#   LOG_NETWORK        日志库容器所在网络（与主库不同时需把工具接入它）
# NewAPI 未启用 LOG_SQL_DSN 时全部留空，工具自动回落主库（向后兼容）。
#######################################
detect_log_database() {
  LOG_SQL_DSN_FINAL=""
  LOG_NETWORK=""

  local raw=""
  raw="$(docker_inspect_env_value "$NEWAPI_CONTAINER" 'LOG_SQL_DSN' || true)"
  if [[ -z "$raw" ]]; then
    log_info "NewAPI 未启用日志分库（无 LOG_SQL_DSN），工具将从主库读取日志"
    return 0
  fi
  log_info "检测到 NewAPI 启用了日志分库（LOG_SQL_DSN），开始配置独立日志库连接..."

  local engine host port db
  engine="$(extract_dsn_engine "$raw")" || die "LOG_SQL_DSN 不受支持或格式不安全；原始 DSN 已隐藏"
  host="$(extract_dsn_host "$raw")" || die "无法从 LOG_SQL_DSN 安全解析 host；原始 DSN 已隐藏"
  port="$(extract_dsn_port "$raw")" || die "无法从 LOG_SQL_DSN 安全解析端口；原始 DSN 已隐藏"
  db="$(extract_dsn_dbname "$raw")" || die "无法从 LOG_SQL_DSN 安全解析数据库名；原始 DSN 已隐藏"
  [[ -n "$host" && -n "$db" ]] || die "LOG_SQL_DSN 缺少 host 或 dbname；原始 DSN 已隐藏"
  if [[ "$engine" == "mysql" ]]; then port="${port:-3306}"; else engine="postgres"; port="${port:-5432}"; fi

  # 与主库同款的连接方式改写
  if [[ "$host" == "127.0.0.1" || "$host" == "localhost" || "$host" == "::1" ]]; then
    local _hit _net _name _port
    _hit="$(find_container_by_published_port "$host" "$port")"
    _net="$(printf '%s' "$_hit" | cut -f1)"; _name="$(printf '%s' "$_hit" | cut -f2)"; _port="$(printf '%s' "$_hit" | cut -f3)"
    if [[ -n "$_net" && -n "$_name" ]]; then
      log_warn "日志库 ${host}:${port} 实为容器 '${_name}'（端口仅发布在宿主机回环）"
      log_info "将把 newapi-tools 接入网络 '${_net}'，用容器名 '${_name}:${_port}' 直连"
      host="$_name"; port="$_port"; LOG_NETWORK="$_net"
    else
      host="host.docker.internal"
      log_info "日志库在宿主机回环上，改写为 host.docker.internal"
    fi
  elif is_ipv4_literal "$host"; then
    local _hit _net _name
    _hit="$(find_container_by_network_ip "$host")"
    _net="$(printf '%s' "$_hit" | cut -f1)"; _name="$(printf '%s' "$_hit" | cut -f2)"
    if [[ -n "$_net" && -n "$_name" ]]; then
      log_warn "日志库 ${host} 是容器 '${_name}' 在网络 '${_net}' 上的 IP"
      log_info "将把 newapi-tools 接入网络 '${_net}'，用容器名直连"
      host="$_name"; LOG_NETWORK="$_net"
    else
      log_info "日志库地址 ${host} 是 IP 但不属于已知 docker 网络容器，按外部地址原样使用"
    fi
  else
    log_info "日志库地址为主机名/外部地址，原样使用: ${host}"
  fi

  LOG_SQL_DSN_FINAL="$(rewrite_dsn_host_port "$raw" "$host" "$port")" ||
    die "无法安全改写 LOG_SQL_DSN 的数据库地址；原始 DSN 已隐藏"

  # 日志库网络与主库不同时，追加日志叠加层并接入网络（相同则工具已在该网络上）
  if [[ -n "$LOG_NETWORK" && "$LOG_NETWORK" != "${NEWAPI_NETWORK:-}" ]]; then
    if [[ -f "$COMPOSE_LOGDB_FILE" ]]; then
      COMPOSE_FILES+=("-f" "$COMPOSE_LOGDB_FILE")
      log_success "已启用日志库网络叠加层（network=${LOG_NETWORK}）"
    else
      log_warn "未找到 $COMPOSE_LOGDB_FILE，日志库网络持久化可能失效（仍会用 docker network connect 兜底）"
    fi
  elif [[ -n "$LOG_NETWORK" ]]; then
    log_info "日志库与主库同在网络 '${LOG_NETWORK}'，无需额外网络配置"
    LOG_NETWORK=""  # 已通过主库网络可达，无需重复接入
  fi

  log_success "检测到日志分库: ${engine} @ ${host}:${port}/${db}${LOG_NETWORK:+ (网络: ${LOG_NETWORK})}"
}

#######################################
# 交互式配置
#######################################
interactive_config() {
  log_info "开始配置..."
  echo ""

  AUTO_GENERATED_PASSWORD=false

  # 前端访问密码
  if [[ -z "${ADMIN_PASSWORD:-}" ]]; then
    echo -e "${YELLOW}请设置前端访问密码${NC} ${BLUE}(直接回车自动生成 32 位强密码)${NC}:"
    read -srp "密码: " ADMIN_PASSWORD
    echo ""

    if [[ -z "$ADMIN_PASSWORD" ]]; then
      ADMIN_PASSWORD="$(generate_strong_password)"
      AUTO_GENERATED_PASSWORD=true
      log_success "已自动生成强密码（部署完成后会显示，请妥善保存）"
    else
      while true; do
        read -srp "确认密码: " ADMIN_PASSWORD_CONFIRM
        echo ""
        if [[ "$ADMIN_PASSWORD" == "$ADMIN_PASSWORD_CONFIRM" ]]; then
          break
        fi
        log_error "两次输入的密码不一致，请重新输入"
        echo ""
        read -srp "密码: " ADMIN_PASSWORD
        echo ""
        if [[ -z "$ADMIN_PASSWORD" ]]; then
          ADMIN_PASSWORD="$(generate_strong_password)"
          AUTO_GENERATED_PASSWORD=true
          log_success "已自动生成强密码"
          break
        fi
      done
    fi
  fi
  log_success "前端密码已设置"

  # API Key 自动生成
  API_KEY="${API_KEY:-$(openssl rand -hex 32 2>/dev/null || head -c 64 /dev/urandom | xxd -p | tr -d '\n' | head -c 64)}"

  # 前端端口默认 1145
  FRONTEND_PORT="${FRONTEND_PORT:-1145}"

  # 前端端口暴露范围
  if [[ -z "${FRONTEND_BIND:-}" ]]; then
    echo ""
    echo -e "${YELLOW}前端端口暴露范围${NC}"
    echo -e "  ${GREEN}1) 仅本机${NC}      仅监听 127.0.0.1，需自行配宿主机 nginx / Caddy 反代到 HTTPS 域名 ${GREEN}(默认，推荐)${NC}"
    echo -e "  ${YELLOW}2) 公网可达${NC}    显式监听 0.0.0.0；必须置于 HTTPS 反向代理、访问控制和防火墙之后"
    read -r -p "请选择 [1/2，回车默认 1]: " bind_choice
    case "$bind_choice" in
      2)
        FRONTEND_BIND="0.0.0.0"
        log_warn "已显式选择公网模式；请勿使用明文 HTTP，必须配置 HTTPS 反向代理和访问控制"
        ;;
      *)
        FRONTEND_BIND="127.0.0.1"
        log_info "已选仅本机模式，部署完成后请配置宿主机 HTTPS 反向代理"
        ;;
    esac
  fi

  # Go 仅在直接对端命中此精确列表时解析 X-Forwarded-For。
  # 合并镜像的内层 Nginx 通过 loopback 访问后端。
  TRUSTED_PROXY_CIDRS="${TRUSTED_PROXY_CIDRS:-127.0.0.1/32,::1/128}"

  echo ""
}

ensure_container_on_network() {
	local container="$1" network="$2" label="$3"
	[[ -n "$network" ]] || return 0
	if container_is_on_network "$container" "$network"; then
		return 0
	fi
	log_info "连接到${label}: $network"
	if ! docker network connect "$network" "$container" 2>/dev/null &&
		! container_is_on_network "$container" "$network"; then
		die "无法连接到${label} '$network'，请检查网络是否存在以及 Docker 权限"
	fi
	container_is_on_network "$container" "$network" || die "连接${label} '$network' 后验证失败"
	log_success "已连接到${label}: $network"
}

#######################################
# 生成 .env 文件
#######################################
generate_env_file() {
  log_info "生成配置文件: $ENV_FILE"

  # 构建 SQL_DSN
  local sql_dsn=""
  if [[ -n "${SOURCE_SQL_DSN:-}" ]]; then
    sql_dsn="$(rewrite_dsn_host_port "$SOURCE_SQL_DSN" "$DB_DNS" "$DB_PORT")" ||
      die "无法安全改写 NewAPI 数据库 DSN；原始 DSN 已隐藏"
  elif [[ "$DB_ENGINE" == "postgres" ]]; then
    sql_dsn="host=${DB_DNS} port=${DB_PORT} user=${DB_USER} password=${DB_PASSWORD} dbname=${DB_NAME} sslmode=disable"
  elif [[ "$DB_ENGINE" == "mysql" ]]; then
    sql_dsn="${DB_USER}:${DB_PASSWORD}@tcp(${DB_DNS}:${DB_PORT})/${DB_NAME}?charset=utf8mb4&parseTime=True"
  fi
	local jwt_secret
	jwt_secret="$(openssl rand -hex 32 2>/dev/null || head -c 64 /dev/urandom | xxd -p | tr -d '\n' | head -c 64)"

  # 给生成的 .env 标记网络模式，方便后续 status / 故障排查辨认
  local network_mode_tag="custom"
  [[ "$USE_BRIDGE_MODE" == "true" ]] && network_mode_tag="bridge"
  [[ "$USE_HOST_MODE" == "true" ]] && network_mode_tag="host"

  cat > "$ENV_FILE" <<EOF
# NewAPI Middleware Tool 配置文件
# 由 deploy.sh 自动生成于 $(date '+%Y-%m-%d %H:%M:%S')
# 网络部署模式: ${network_mode_tag}

# NewAPI 环境
NEWAPI_CONTAINER=${NEWAPI_CONTAINER}
NEWAPI_NETWORK=${NEWAPI_NETWORK}
# 供 install.sh 在容器重建后恢复手动附加的网络
NEWAPI_NETWORK_MODE=${network_mode_tag}
NEWAPI_ORIGINAL_NETWORK=${ORIGINAL_NETWORK:-}
# 仅当 NewAPI 容器明确配置 REDIS_CONN_STRING=（空）时为 true；未知或非空均为 false
NEWAPI_REDIS_DISABLED=${NEWAPI_REDIS_DISABLED:-false}
# 高风险兼容开关：默认禁止不完整的直接数据库永久删除，请优先使用 NewAPI 管理 API
ALLOW_UNSAFE_HARD_DELETE=false
# 隐私敏感：默认不持续覆盖用户的 record_ip_log 设置
ENFORCE_IP_RECORDING=false

# 数据库配置 (Go 版本推荐 SQL_DSN)
SQL_DSN=$(dotenv_quote "$sql_dsn")
DB_ENGINE=${DB_ENGINE}
DB_DNS=${DB_DNS}
DB_PORT=${DB_PORT}
DB_NAME=$(dotenv_quote "$DB_NAME")
DB_USER=$(dotenv_quote "$DB_USER")
DB_PASSWORD=$(dotenv_quote "$DB_PASSWORD")

# 日志分库 (NewAPI 启用 LOG_SQL_DSN 时自动检测；为空则日志查询回落主库)
LOG_SQL_DSN=$(dotenv_quote "${LOG_SQL_DSN_FINAL:-}")
# 日志库容器所在网络 (与主库不同时由 docker-compose.logdb.yml 叠加层接入)
LOG_NETWORK=${LOG_NETWORK:-}

# 认证配置
ADMIN_PASSWORD=$(dotenv_quote "$ADMIN_PASSWORD")
API_KEY=$(dotenv_quote "$API_KEY")

# 服务配置
FRONTEND_PORT=${FRONTEND_PORT}
FRONTEND_BIND=${FRONTEND_BIND}
# 外层反向代理存在时，请追加内层 Nginx 实际看到的代理 IP（精确 /32 或 /128）
TRUSTED_PROXY_CIDRS=${TRUSTED_PROXY_CIDRS}
TIMEZONE=Asia/Shanghai
LOG_LEVEL=info

# JWT 配置
JWT_SECRET=$(dotenv_quote "$jwt_secret")
JWT_EXPIRE_HOURS=24

# Redis 配置
REDIS_PASSWORD=''
EOF

  chmod 600 "$ENV_FILE"
  log_success "配置文件已生成"
}

#######################################
# 检查 docker-compose.yml 是否存在
#######################################
check_compose_file() {
  if [[ ! -f "$COMPOSE_FILE" ]]; then
    die "找不到 docker-compose.yml 文件，请确保在项目根目录运行此脚本"
  fi
  log_success "找到 Docker Compose 配置文件"
}

#######################################
# 下载 GeoIP 数据库
#######################################
download_geoip_database() {
  local geoip_dir="${SCRIPT_DIR}/data/geoip"
  local city_db="${geoip_dir}/GeoLite2-City.mmdb"
  local asn_db="${geoip_dir}/GeoLite2-ASN.mmdb"

  # 如果数据库已存在，跳过下载
  if [[ -f "$city_db" && -f "$asn_db" ]]; then
    log_success "GeoIP 数据库已存在，跳过下载"
    return 0
  fi

  log_info "正在下载 GeoIP 数据库..."
  mkdir -p "$geoip_dir"

  # 下载源（优先 GitHub 直链，备用国内镜像）
  local base_url="https://raw.githubusercontent.com/adysec/IP_database/main/geolite"
  local fallback_url="https://raw.gitmirror.com/adysec/IP_database/main/geolite"

  # 下载 City 数据库
  if [[ ! -f "$city_db" ]]; then
    log_info "下载 GeoLite2-City.mmdb..."
    if ! curl -sL --connect-timeout 15 -o "$city_db" "${base_url}/GeoLite2-City.mmdb" 2>/dev/null; then
      log_warn "GitHub 下载失败，尝试国内镜像..."
      curl -sL --connect-timeout 30 -o "$city_db" "${fallback_url}/GeoLite2-City.mmdb" 2>/dev/null || {
        log_warn "GeoLite2-City.mmdb 下载失败，IP 地理位置功能可能不可用"
        rm -f "$city_db"
      }
    fi
  fi

  # 下载 ASN 数据库
  if [[ ! -f "$asn_db" ]]; then
    log_info "下载 GeoLite2-ASN.mmdb..."
    if ! curl -sL --connect-timeout 15 -o "$asn_db" "${base_url}/GeoLite2-ASN.mmdb" 2>/dev/null; then
      log_warn "GitHub 下载失败，尝试国内镜像..."
      curl -sL --connect-timeout 30 -o "$asn_db" "${fallback_url}/GeoLite2-ASN.mmdb" 2>/dev/null || {
        log_warn "GeoLite2-ASN.mmdb 下载失败，ASN 查询功能可能不可用"
        rm -f "$asn_db"
      }
    fi
  fi

  # 检查下载结果
  if [[ -f "$city_db" && -f "$asn_db" ]]; then
    log_success "GeoIP 数据库下载完成"
  else
    log_warn "部分 GeoIP 数据库下载失败，可稍后手动下载"
  fi
}

#######################################
# 启动服务
#######################################
start_services() {
  log_info "启动服务..."

  # 下载 GeoIP 数据库
  download_geoip_database

  # 检查是否有旧容器
  if docker ps -a --format '{{.Names}}' | grep -qE '^newapi-tools$'; then
    log_warn "发现已存在的服务容器，正在停止..."
    $DOCKER_COMPOSE "${COMPOSE_FILES[@]}" --env-file "$ENV_FILE" down 2>/dev/null || true
  fi

  # 拉取最新镜像
  log_info "拉取最新镜像..."
  $DOCKER_COMPOSE "${COMPOSE_FILES[@]}" --env-file "$ENV_FILE" pull

  # 启动服务
  $DOCKER_COMPOSE "${COMPOSE_FILES[@]}" --env-file "$ENV_FILE" up -d

  # 将容器连接到 NewAPI 网络（用于访问数据库）
  # 注意：docker-compose.yml 中也配置了网络，这里是双重保障。
  # 默认 bridge 模式使用数据库容器的 bridge IPv4，因此必须额外接回原始 bridge。
  if [[ "$USE_HOST_MODE" == "true" ]]; then
    log_info "host 模式：跳过 docker network connect"
  elif [[ "$USE_BRIDGE_MODE" == "true" ]]; then
    local bridge_network="${ORIGINAL_NETWORK:-bridge}"
    ensure_container_on_network "newapi-tools" "$bridge_network" "NewAPI 原始 bridge 网络"
  elif [[ -n "$NEWAPI_NETWORK" ]]; then
    ensure_container_on_network "newapi-tools" "$NEWAPI_NETWORK" "NewAPI 网络"
  fi

  # 日志库在另一条网络上时，把工具也接入（叠加层已配置，这里双重保障）
  if [[ -n "${LOG_NETWORK:-}" ]]; then
    ensure_container_on_network "newapi-tools" "$LOG_NETWORK" "日志库网络"
  fi

  log_success "服务已启动!"

  # 获取服务器 IP
  local server_ip
  server_ip="$(hostname -I 2>/dev/null | awk '{print $1}')" || server_ip="$(ip route get 1 2>/dev/null | awk '{print $7; exit}')" || server_ip="localhost"

  echo ""
  echo -e "${GREEN}========================================${NC}"
  echo -e "${GREEN}  NewAPI Middleware Tool 部署成功!${NC}"
  echo -e "${GREEN}========================================${NC}"
  echo ""
  if [[ "$FRONTEND_BIND" == "127.0.0.1" || "$FRONTEND_BIND" == "localhost" || "$FRONTEND_BIND" == "::1" ]]; then
    echo -e "${YELLOW}前端端口仅监听本机 127.0.0.1:${FRONTEND_PORT}，外部直连不可达${NC}"
    echo -e "请在宿主机配置 nginx 反代到 HTTPS 域名，参考配置："
    cat <<NGINX
   server {
     listen 443 ssl http2;
     server_name your-domain.com;
     ssl_certificate     /path/to/fullchain.pem;
     ssl_certificate_key /path/to/privkey.pem;
     location / {
       proxy_pass http://127.0.0.1:${FRONTEND_PORT};
       proxy_set_header Host \$host;
       proxy_set_header X-Real-IP \$remote_addr;
       # 覆盖客户端自带的 X-Forwarded-For，防止伪造限流身份。
       proxy_set_header X-Forwarded-For \$remote_addr;
       proxy_set_header X-Forwarded-Proto \$scheme;
     }
   }
NGINX
    echo -e "${YELLOW}提示：若内层 Nginx 看到的外层代理来源不是 loopback，请把该精确 IP（/32 或 /128）追加到 .env 的 TRUSTED_PROXY_CIDRS。${NC}"
  else
    echo -e "前端访问地址: ${BLUE}http://${server_ip}:${FRONTEND_PORT}${NC}"
    echo -e "API 地址: ${BLUE}http://${server_ip}:${FRONTEND_PORT}/api${NC}"
  fi
  echo ""

  if [[ "${AUTO_GENERATED_PASSWORD:-false}" == "true" ]]; then
    local sep
    sep="$(printf '═%.0s' {1..62})"
    echo -e "${YELLOW}╔${sep}╗${NC}"
    printf "${YELLOW}║${NC}  ${YELLOW}⚠  以下是自动生成的随机登录密码，请立即复制保存：${NC}        ${YELLOW}║${NC}\n"
    printf "${YELLOW}╠${sep}╣${NC}\n"
    printf "${YELLOW}║${NC}                                                                ${YELLOW}║${NC}\n"
    printf "${YELLOW}║${NC}    ${GREEN}%-56s${NC}    ${YELLOW}║${NC}\n" "$ADMIN_PASSWORD"
    printf "${YELLOW}║${NC}                                                                ${YELLOW}║${NC}\n"
    printf "${YELLOW}╠${sep}╣${NC}\n"
    printf "${YELLOW}║${NC}  忘记密码可重新运行 install.sh，管理面板内会显示该密码         ${YELLOW}║${NC}\n"
    printf "${YELLOW}║${NC}  也可执行: grep ADMIN_PASSWORD %-32s${YELLOW}║${NC}\n" "$ENV_FILE"
    echo -e "${YELLOW}╚${sep}╝${NC}"
  else
    echo -e "登录密码: ${YELLOW}${ADMIN_PASSWORD}${NC}"
  fi
  echo ""
  echo -e "配置文件: ${ENV_FILE}"
  echo -e "Compose 文件: ${COMPOSE_FILE}"
  echo ""
  echo -e "查看日志: ${YELLOW}cd ${SCRIPT_DIR} && docker compose logs -f${NC}"
  echo ""
}

#######################################
# 卸载服务
#######################################
uninstall() {
  log_warn "正在卸载 NewAPI Middleware Tool..."

  if [[ -f "$COMPOSE_FILE" && -f "$ENV_FILE" ]]; then
    $DOCKER_COMPOSE -f "$COMPOSE_FILE" --env-file "$ENV_FILE" down -v 2>/dev/null || true
    log_success "容器已停止并移除"
  fi

  if [[ -f "$ENV_FILE" ]]; then
    rm -f "$ENV_FILE"
    log_success "配置文件已删除"
  fi

  log_success "卸载完成"
}

#######################################
# 查看状态
#######################################
show_status() {
  log_info "服务状态:"
  echo ""

  if [[ -f "$COMPOSE_FILE" && -f "$ENV_FILE" ]]; then
    $DOCKER_COMPOSE -f "$COMPOSE_FILE" --env-file "$ENV_FILE" ps
  else
    log_warn "未找到配置文件，服务可能未部署"
  fi
}

#######################################
# 显示帮助
#######################################
show_help() {
  cat <<EOF
NewAPI Middleware Tool - 一键部署脚本

用法:
  ./deploy.sh              交互式部署
  ./deploy.sh --uninstall  卸载服务
  ./deploy.sh --status     查看服务状态
  ./deploy.sh --help       显示帮助

环境变量:
  NEWAPI_CONTAINER   指定 NewAPI 容器名 (默认: 自动检测)
  NEWAPI_NETWORK     指定 Docker 网络名 (默认: 自动检测)
  ADMIN_PASSWORD     前端访问密码 (默认: 交互式输入)
  API_KEY            后端 API Key (默认: 交互式输入或自动生成)
  FRONTEND_PORT      前端端口 (默认: 1145)
  FRONTEND_BIND      前端端口绑定网卡 0.0.0.0/127.0.0.1 (默认: 交互式选择)
  TRUSTED_PROXY_CIDRS 允许解析其 XFF 的精确代理 IP/CIDR (默认: loopback)

示例:
  # 基本部署
  ./deploy.sh

  # 指定容器名部署
  NEWAPI_CONTAINER=my-newapi ./deploy.sh

  # 非交互式部署，用 nginx 反代模式
  ADMIN_PASSWORD=mypass API_KEY=mykey FRONTEND_BIND=127.0.0.1 ./deploy.sh
EOF
}

#######################################
# 主函数
#######################################
main() {
  need_cmd docker
  detect_docker_compose

  local mode="${1:-}"

  case "$mode" in
    --help|-h)
      show_help
      exit 0
      ;;
    --uninstall)
      uninstall
      exit 0
      ;;
    --status)
      show_status
      exit 0
      ;;
    "")
      # 正常部署流程
      echo ""
      echo -e "${BLUE}========================================${NC}"
      echo -e "${BLUE}  NewAPI Middleware Tool 部署脚本${NC}"
      echo -e "${BLUE}========================================${NC}"
      echo ""

      detect_environment
      detect_log_database
      interactive_config
      generate_env_file
      check_compose_file
      start_services
      ;;
    *)
      die "未知参数: $mode (使用 --help 查看帮助)"
      ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
