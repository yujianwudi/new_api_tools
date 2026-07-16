#!/usr/bin/env bash
set -euo pipefail

#######################################
# NewAPI Middleware Tool - 日志分库（LOG_SQL_DSN）兼容脚本
#
# 背景：
#   NewAPI 的部分 fork 支持 LOG_SQL_DSN，把 logs 表整张分离到
#   独立数据库。此时主库的 logs 表会被冻结、不再更新，本工具若只连主库就读不到
#   实时日志（dashboard 流量分析、使用日志、模型监控、风控/IP 分析全为 0）。
#
#   本脚本「只」负责日志库这一特例，刻意不动通用 deploy.sh：
#     1. 从 NewAPI 容器读取 LOG_SQL_DSN
#     2. 解析它，并对「数据库是容器、端口只发布在宿主机回环」「数据库是某条
#        bridge 网络上容器的 IP」等情形做容器名/网络改写（与 deploy.sh 同款逻辑）
#     3. 把改写后的 LOG_SQL_DSN 写进本工具的 .env，必要时把工具容器接入日志库网络
#     4. 重建本工具容器使其生效
#
# 用法：
#   ./setup-log-db.sh            # 交互式：检测 + 写入 .env + 重建
#   ./setup-log-db.sh --print    # 只检测并打印脱敏后的连接目标，不改任何东西
#   ./setup-log-db.sh --no-restart  # 写入 .env 但不自动重建容器
#
# 环境变量：
#   NEWAPI_CONTAINER   指定 NewAPI 容器名（默认自动检测）
#   LOG_SQL_DSN        直接指定日志库 DSN（跳过从 NewAPI 容器读取）
#######################################

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || echo "")"
TOOLS_CONTAINER="newapi-tools"
# ENV_FILE / COMPOSE_FILE 在 resolve_project_dir() 中确定（支持 curl 直跑）
ENV_FILE=""
COMPOSE_FILE=""

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
log_info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[OK]${NC} $*"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
die()         { log_error "$*"; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || die "缺少必要命令: $1"; }

detect_docker_compose() {
  local output version
  output="$(docker compose version 2>/dev/null)" ||
    die "缺少 Docker Compose v2.24+ 插件；不再支持 legacy docker-compose v1"
  if [[ "$output" =~ v?([0-9]+)\.([0-9]+)\.([0-9]+) ]]; then
    version="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${BASH_REMATCH[3]}"
  else
    die "无法解析 Docker Compose 版本；要求 v2.24+"
  fi
  version_at_least "$version" "2.24.0" ||
    die "Docker Compose ${version} 过旧；日志库 overlay 要求 v2.24+"
  DOCKER_COMPOSE="docker compose"
}

version_at_least() {
  local current="$1" required="$2"
  local current_major current_minor current_patch required_major required_minor required_patch
  IFS=. read -r current_major current_minor current_patch <<< "$current"
  IFS=. read -r required_major required_minor required_patch <<< "$required"
  (( current_major > required_major )) ||
    (( current_major == required_major && current_minor > required_minor )) ||
    (( current_major == required_major && current_minor == required_minor && current_patch >= required_patch ))
}

#######################################
# 定位本工具的项目目录（含 .env 与 docker-compose.yml）。
# 支持三种运行方式：
#   1. 在项目目录里直接 ./setup-log-db.sh        → 用脚本所在目录
#   2. bash <(curl ...) 一键运行（无文件路径）   → 从运行中的 newapi-tools 容器
#      的 compose 工作目录标签反查，或扫描常见路径
#######################################
resolve_project_dir() {
  local candidates=()

  # (a) 脚本自身所在目录（普通 clone 后执行）
  if [[ -n "$SCRIPT_DIR" && -f "${SCRIPT_DIR}/docker-compose.yml" ]]; then
    candidates+=("$SCRIPT_DIR")
  fi

  # (b) 从运行中的 newapi-tools 容器读取 compose 工作目录标签
  local wd
  wd="$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' "$TOOLS_CONTAINER" 2>/dev/null || true)"
  [[ -n "$wd" && -f "${wd}/docker-compose.yml" ]] && candidates+=("$wd")

  # (c) 当前工作目录
  [[ -f "./docker-compose.yml" ]] && candidates+=("$(pwd)")

  # (d) 常见安装路径
  local guess
  for guess in "$HOME/new_api_tools" "/root/new-api/new_api_tools" "/opt/new_api_tools" "./new_api_tools"; do
    [[ -f "${guess}/docker-compose.yml" ]] && candidates+=("$guess")
  done

  local dir
  for dir in "${candidates[@]}"; do
    if [[ -f "${dir}/.env" ]]; then
      PROJECT_DIR="$dir"; ENV_FILE="${dir}/.env"; COMPOSE_FILE="${dir}/docker-compose.yml"
      return 0
    fi
  done
  # 没找到带 .env 的；退而取第一个有 compose 的目录（.env 可能尚未生成）
  if [[ ${#candidates[@]} -gt 0 ]]; then
    PROJECT_DIR="${candidates[0]}"; ENV_FILE="${PROJECT_DIR}/.env"; COMPOSE_FILE="${PROJECT_DIR}/docker-compose.yml"
    return 0
  fi
  die "找不到 NewAPI-Tool 项目目录（含 docker-compose.yml/.env）。请 cd 到该目录后再运行本脚本。"
}

#######################################
# DSN parsing and rewriting. Keep this contract aligned with deploy.sh:
# recognize only unambiguous PostgreSQL/MySQL forms, preserve credentials and
# query options byte-for-byte, and change only the network host/port.
#######################################
dsn_parse_error() {
  printf '无法安全解析日志库 DSN：%s（原始 DSN 已隐藏）\n' "$1" >&2
  return 1
}

dsn_validate_port() {
  local port="${1:-}"
  [[ -z "$port" ]] && return 0
  if [[ ! "$port" =~ ^[0-9]+$ || ${#port} -gt 5 ]] ||
    (( 10#$port < 1 || 10#$port > 65535 )); then
    dsn_parse_error "端口必须是 1-65535 的数字"
  fi
}

dsn_validate_postgres_url_query() {
  local query="${1:-}" parameter key normalized_key
  [[ -n "$query" ]] || return 0
  while :; do
    parameter="${query%%&*}"
    key="${parameter%%=*}"
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
  # libpq quoting requires a full lexer. Reject ambiguous input instead of
  # splitting or rewriting credentials incorrectly.
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

#######################################
# Docker 探测（与 deploy.sh 同款思路）
#######################################
detect_newapi_container() {
  local found=""
  found="$(docker ps --format '{{.Names}}' | awk 'tolower($0) ~ /(^|[-_])new-api([-_]|$)/ {print; exit}')"
  [[ -n "$found" ]] && { echo "$found"; return 0; }
  found="$(docker ps --format '{{.ID}}\t{{.Image}}' | awk 'tolower($2) ~ /(^|\/)new-api([-_:]|$)/ {print $1; exit}')"
  [[ -n "$found" ]] && { echo "$found"; return 0; }
  return 1
}

docker_inspect_env_value() {
  docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$1" 2>/dev/null \
    | awk -v k="$2" 'index($0, k "=") == 1 { print substr($0, length(k) + 2); exit }'
}

is_ipv4_literal() { [[ "${1:-}" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; }
is_ip_literal() { is_ipv4_literal "${1:-}" || [[ "${1:-}" == *:* ]]; }

# 找「把指定宿主机端口发布到回环/0.0.0.0」的容器 → "网络<TAB>容器名<TAB>容器内端口"
find_container_by_published_port() {
  local target_port="${1:-}"; [[ -z "$target_port" ]] && return 0
  local cid name net cport
  while IFS= read -r cid; do
    [[ -z "$cid" ]] && continue
    cport="$(docker port "$cid" 2>/dev/null | awk -F' -> ' -v p="$target_port" '
      { n=split($2,h,":"); if (h[n]==p) { split($1,c,"/"); print c[1]; exit } }')"
    [[ -z "$cport" ]] && continue
    name="$(docker inspect -f '{{.Name}}' "$cid" 2>/dev/null | sed 's#^/##')"
    while IFS= read -r net; do
      [[ -z "$net" ]] && continue
      case "$net" in bridge|host|none) continue ;; esac
      printf '%s\t%s\t%s\n' "$net" "$name" "$cport"; return 0
    done < <(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{println $k}}{{end}}' "$cid" 2>/dev/null)
  done < <(docker ps -q)
  return 0
}

# 给定 IP，反查持有它的容器 → "网络<TAB>容器名"
find_container_by_network_ip() {
  local target_ip="${1:-}"; [[ -z "$target_ip" ]] && return 0
  local cid name net ip
  while IFS= read -r cid; do
    [[ -z "$cid" ]] && continue
    name="$(docker inspect -f '{{.Name}}' "$cid" 2>/dev/null | sed 's#^/##')"
    while IFS=$'\t' read -r net ip; do
      [[ -z "$net" ]] && continue
      [[ "$ip" == "$target_ip" ]] && { printf '%s\t%s\n' "$net" "$name"; return 0; }
    done < <(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{if $v.IPAddress}}{{$k}}{{"\t"}}{{$v.IPAddress}}{{"\n"}}{{end}}{{if $v.GlobalIPv6Address}}{{$k}}{{"\t"}}{{$v.GlobalIPv6Address}}{{"\n"}}{{end}}{{end}}' "$cid" 2>/dev/null)
  done < <(docker ps -q)
  return 0
}

# 把工具容器接入指定网络（幂等）
ensure_tool_on_network() {
  local net="$1"
  docker network inspect "$net" >/dev/null 2>&1 || die "网络 '$net' 不存在"
  if docker ps -a --format '{{.Names}}' | grep -qx "$TOOLS_CONTAINER"; then
    docker network connect "$net" "$TOOLS_CONTAINER" 2>/dev/null \
      && log_success "已把 $TOOLS_CONTAINER 接入网络 $net" \
      || log_info "$TOOLS_CONTAINER 已在网络 $net 上"
  fi
}

# 读取 .env 里某 key 的值（无则空）
read_env_value() {
  local key="$1" value
  [[ -f "$ENV_FILE" && ! -L "$ENV_FILE" ]] || { echo ""; return 0; }
  value="$(awk -v k="$key" '
    index($0, k "=") == 1 { value = substr($0, length(k) + 2); found = 1 }
    END { if (found) print value }
  ' "$ENV_FILE")"
  value="${value%$'\r'}"
  if [[ ${#value} -ge 2 && "$value" == \'*\' ]]; then
    value="${value:1:${#value}-2}"
    value="${value//\\\'/\'}"
  elif [[ ${#value} -ge 2 && "$value" == \"*\" ]]; then
    value="${value:1:${#value}-2}"
  fi
  printf '%s\n' "$value"
}

env_file_has_key() {
  local key="$1"
  [[ -f "$ENV_FILE" && ! -L "$ENV_FILE" ]] || return 1
  grep -qE "^${key}=" "$ENV_FILE"
}

dotenv_quote() {
  local value="${1-}" escaped
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || return 1
  escaped="$(printf '%s' "$value" | sed "s/'/\\\\'/g")"
  printf "'%s'" "$escaped"
}

# Persist a complete file image in the target directory. Both .env and the
# generated Compose override can contain deployment-sensitive data, so never
# expose a truncate/append window and never follow an existing symlink.
setup_target_identity() {
  local target="$1"
  [[ -f "$target" && ! -L "$target" ]] || return 1
  stat -Lc '%d:%i' -- "$target" 2>/dev/null
}

setup_target_matches_identity() {
  local target="$1" expected="$2" current
  if [[ "$expected" == "absent" ]]; then
    [[ ! -e "$target" && ! -L "$target" ]]
    return
  fi
  current="$(setup_target_identity "$target")" || return 1
  [[ "$current" == "$expected" ]]
}

SETUP_FILE_CONTENT=""
SETUP_FILE_IDENTITY=""
load_setup_file_image() {
  local target="$1" file_fd fd_path path_identity fd_identity status
  [[ -f "$target" && ! -L "$target" && -r "$target" ]] || return 1
  exec {file_fd}< "$target" || return 1
  fd_path="/proc/${BASHPID}/fd/${file_fd}"
  if [[ ! -e "$fd_path" ]] ||
    ! path_identity="$(stat -Lc '%d:%i' -- "$target" 2>/dev/null)" ||
    ! fd_identity="$(stat -Lc '%d:%i' -- "$fd_path" 2>/dev/null)" ||
    [[ "$path_identity" != "$fd_identity" ]] ||
    [[ -L "$target" ]]; then
    exec {file_fd}<&-
    return 1
  fi
  SETUP_FILE_CONTENT="$(cat <&"$file_fd")"
  status=$?
  exec {file_fd}<&-
  (( status == 0 )) || return "$status"
  SETUP_FILE_IDENTITY="$path_identity"
}

atomic_write_setup_file() {
  local target="$1" content="$2" expected_identity="${3:-}" parent tmp mode
  parent="$(dirname "$target")"
  [[ -d "$parent" ]] || return 1
  if [[ -z "$expected_identity" ]]; then
    if [[ -e "$target" || -L "$target" ]]; then
      expected_identity="$(setup_target_identity "$target")" || return 1
    else
      expected_identity="absent"
    fi
  elif ! setup_target_matches_identity "$target" "$expected_identity"; then
    return 1
  fi

  tmp="$(umask 077; mktemp "${target}.tmp.XXXXXX")" || return 1
  if ! printf '%s\n' "$content" > "$tmp" ||
    ! chmod 600 "$tmp" ||
    ! sync -f "$tmp" ||
    ! setup_target_matches_identity "$target" "$expected_identity" ||
    ! mv -Tf -- "$tmp" "$target" ||
    ! sync -f "$parent"; then
    rm -f -- "$tmp"
    return 1
  fi

  [[ -f "$target" && ! -L "$target" ]] || return 1
  mode="$(stat -c '%a' -- "$target" 2>/dev/null)" || return 1
  [[ "$mode" == "600" ]]
}

LOG_ENV_ATTEMPT_CONTENT=""
LOG_ENV_ATTEMPT_READY=false
persist_log_db_env() {
  local dsn="$1" log_network="${2:-}" required_identity="${3:-}"
  local content quoted_dsn quoted_network
  load_setup_file_image "$ENV_FILE" || return 1
  if [[ -n "$required_identity" && "$SETUP_FILE_IDENTITY" != "$required_identity" ]]; then
    return 1
  fi
  [[ "$log_network" != *$'\n'* && "$log_network" != *$'\r'* ]] || return 1
  quoted_dsn="$(dotenv_quote "$dsn")" || return 1
  quoted_network="$(dotenv_quote "$log_network")" || return 1

  content="$(
    awk '
      index($0, "LOG_SQL_DSN=") == 1 { next }
      index($0, "LOG_NETWORK=") == 1 { next }
      { print }
    ' <<< "$SETUP_FILE_CONTENT"
    printf 'LOG_SQL_DSN=%s\n' "$quoted_dsn"
    printf 'LOG_NETWORK=%s\n' "$quoted_network"
  )" || return 1

  LOG_ENV_ATTEMPT_CONTENT="$content"
  LOG_ENV_ATTEMPT_READY=true
  atomic_write_setup_file "$ENV_FILE" "$content" "$SETUP_FILE_IDENTITY"
}

# 写 docker-compose.override.yml，把日志库网络固化进工具服务。
# docker compose 默认会自动叠加 override 文件，使「纯 docker compose up」重建后
# 网络依然挂着（setup-log-db.sh 自己用纯 compose 重建，故能吃到它）。
# 同时把 LOG_NETWORK 写入 .env，供 deploy.sh / install.sh 通过
# docker-compose.logdb.yml 恢复同一网络；override 保留纯 Compose 的兼容入口。
LOG_OVERRIDE_NET_NAME="log-db-network"
LOG_OVERRIDE_MARKER="# 由 setup-log-db.sh 自动生成：把 newapi-tools 接入日志库所在的 docker 网络，"
LOG_OVERRIDE_SNAPSHOT_EXISTS=false
LOG_OVERRIDE_SNAPSHOT_CONTENT=""
LOG_OVERRIDE_SNAPSHOT_IDENTITY=""
LOG_OVERRIDE_ATTEMPT_CONTENT=""
LOG_OVERRIDE_ATTEMPT_READY=false
LOG_OVERRIDE_ATTEMPT_REMOVED=false
LOG_ENV_SNAPSHOT_CONTENT=""
LOG_ENV_SNAPSHOT_IDENTITY=""

capture_log_db_env_snapshot() {
  load_setup_file_image "$ENV_FILE" || return 1
  LOG_ENV_SNAPSHOT_CONTENT="$SETUP_FILE_CONTENT"
  LOG_ENV_SNAPSHOT_IDENTITY="$SETUP_FILE_IDENTITY"
}

restore_log_db_env_snapshot() {
  local current_content current_identity
  load_setup_file_image "$ENV_FILE" || return 1
  current_content="$SETUP_FILE_CONTENT"
  current_identity="$SETUP_FILE_IDENTITY"

  if [[ "$current_identity" == "$LOG_ENV_SNAPSHOT_IDENTITY" &&
    "$current_content" == "$LOG_ENV_SNAPSHOT_CONTENT" ]]; then
    return 0
  fi
  [[ "$LOG_ENV_ATTEMPT_READY" == "true" &&
    "$current_content" == "$LOG_ENV_ATTEMPT_CONTENT" ]] || return 1
  atomic_write_setup_file "$ENV_FILE" "$LOG_ENV_SNAPSHOT_CONTENT" "$current_identity"
}

capture_log_network_override_snapshot() {
  local override="${PROJECT_DIR}/docker-compose.override.yml"
  LOG_OVERRIDE_SNAPSHOT_EXISTS=false
  LOG_OVERRIDE_SNAPSHOT_CONTENT=""
  LOG_OVERRIDE_SNAPSHOT_IDENTITY=""
  [[ -e "$override" || -L "$override" ]] || return 0
  load_setup_file_image "$override" || return 1
  LOG_OVERRIDE_SNAPSHOT_EXISTS=true
  LOG_OVERRIDE_SNAPSHOT_CONTENT="$SETUP_FILE_CONTENT"
  LOG_OVERRIDE_SNAPSHOT_IDENTITY="$SETUP_FILE_IDENTITY"
}

restore_log_network_override_snapshot() {
  local override="${PROJECT_DIR}/docker-compose.override.yml"
  local current_exists=false current_content="" current_identity=""
  if [[ -e "$override" || -L "$override" ]]; then
    load_setup_file_image "$override" || return 1
    current_exists=true
    current_content="$SETUP_FILE_CONTENT"
    current_identity="$SETUP_FILE_IDENTITY"
  fi

  if [[ "$LOG_OVERRIDE_SNAPSHOT_EXISTS" == "true" ]]; then
    if [[ "$current_exists" == "true" &&
      "$current_identity" == "$LOG_OVERRIDE_SNAPSHOT_IDENTITY" &&
      "$current_content" == "$LOG_OVERRIDE_SNAPSHOT_CONTENT" ]]; then
      return 0
    fi
    if [[ "$current_exists" == "true" ]]; then
      [[ "$LOG_OVERRIDE_ATTEMPT_READY" == "true" &&
        "$current_content" == "$LOG_OVERRIDE_ATTEMPT_CONTENT" ]] || return 1
      atomic_write_setup_file "$override" "$LOG_OVERRIDE_SNAPSHOT_CONTENT" "$current_identity"
      return
    fi
    [[ "$LOG_OVERRIDE_ATTEMPT_REMOVED" == "true" ]] || return 1
    atomic_write_setup_file "$override" "$LOG_OVERRIDE_SNAPSHOT_CONTENT" "absent"
    return
  fi

  [[ "$current_exists" == "true" ]] || return 0
  [[ "$LOG_OVERRIDE_ATTEMPT_READY" == "true" &&
    "$current_content" == "$LOG_OVERRIDE_ATTEMPT_CONTENT" ]] || return 1
  grep -Fxq "$LOG_OVERRIDE_MARKER" "$override" || return 1
  setup_target_matches_identity "$override" "$current_identity" || return 1
  rm -f -- "$override" || return 1
  [[ ! -e "$override" && ! -L "$override" ]] || return 1
  sync -f "$PROJECT_DIR"
}

write_log_network_override() {
  local net="$1"
  local override="${PROJECT_DIR}/docker-compose.override.yml" content
  local main_net; main_net="$(read_env_value NEWAPI_NETWORK)"

  [[ "$net" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ ]] || {
    log_error "日志库 Docker 网络名格式无效"
    return 1
  }

  # 日志库网络与主库网络相同 → 工具已经在上面，无需 override（避免重复挂同一网络报错）
  if [[ -n "$main_net" && "$net" == "$main_net" ]]; then
    log_info "日志库与主库同在网络 '$net'，无需额外网络配置"
    remove_generated_log_network_override
    return
  fi

  if [[ -e "$override" || -L "$override" ]]; then
    [[ -f "$override" && ! -L "$override" ]] || {
      log_error "现有 Compose override 不是安全的常规文件"
      return 1
    }
    grep -Fxq "$LOG_OVERRIDE_MARKER" "$override" || {
      log_error "现有 Compose override 不是 setup-log-db.sh 生成，拒绝覆盖用户配置"
      return 1
    }
  fi

  content="$(cat <<EOF
# 由 setup-log-db.sh 自动生成：把 newapi-tools 接入日志库所在的 docker 网络，
# 使「docker compose up」重建后日志库连接依然可用。请勿手改；重跑 setup-log-db.sh 会覆盖。
services:
  ${TOOLS_CONTAINER}:
    networks:
      - ${LOG_OVERRIDE_NET_NAME}
networks:
  ${LOG_OVERRIDE_NET_NAME}:
    external: true
    name: '${net}'
EOF
  )"
  LOG_OVERRIDE_ATTEMPT_CONTENT="$content"
  LOG_OVERRIDE_ATTEMPT_READY=true
  atomic_write_setup_file "$override" "$content" || {
    log_error "无法原子持久化日志库 Compose override"
    return 1
  }
  log_success "已写入 $override（固化日志库网络 '${net}'）"
}

remove_generated_log_network_override() {
  local override="${PROJECT_DIR}/docker-compose.override.yml" identity
  [[ -e "$override" || -L "$override" ]] || return 0
  identity="$(setup_target_identity "$override")" || {
    log_error "现有 Compose override 不是安全的常规文件，拒绝自动清理"
    return 1
  }
  if ! grep -Fxq "$LOG_OVERRIDE_MARKER" "$override"; then
    log_warn "保留非 setup-log-db.sh 生成的 $override；请确认其中没有旧日志网络"
    return 0
  fi
  setup_target_matches_identity "$override" "$identity" || {
    log_error "Compose override 在清理前发生变化，拒绝继续"
    return 1
  }
  LOG_OVERRIDE_ATTEMPT_REMOVED=true
  rm -f -- "$override" || { log_error "无法删除旧日志库 Compose override"; return 1; }
  [[ ! -e "$override" && ! -L "$override" ]] || {
    log_error "旧日志库 Compose override 删除未生效"
    return 1
  }
  sync -f "$PROJECT_DIR" || {
    log_error "旧日志库 Compose override 删除后无法同步项目目录"
    return 1
  }
  log_success "已清理不再需要的日志库 Compose override"
}

commit_log_db_configuration() {
  local dsn="$1" persisted_log_network="${2:-}"
  LOG_ENV_ATTEMPT_CONTENT=""
  LOG_ENV_ATTEMPT_READY=false
  LOG_OVERRIDE_ATTEMPT_CONTENT=""
  LOG_OVERRIDE_ATTEMPT_READY=false
  LOG_OVERRIDE_ATTEMPT_REMOVED=false
  capture_log_db_env_snapshot || {
    log_error "现有 .env 不安全或无法读取；配置尚未修改"
    return 1
  }
  capture_log_network_override_snapshot || {
    log_error "现有 Compose override 不安全或无法读取；.env 尚未修改"
    return 1
  }

  if [[ -n "$persisted_log_network" ]]; then
    if ! write_log_network_override "$persisted_log_network"; then
      restore_log_network_override_snapshot ||
        log_error "高危：override 写入失败后无法恢复旧快照"
      return 1
    fi
  elif ! remove_generated_log_network_override; then
    restore_log_network_override_snapshot ||
      log_error "高危：override 清理失败后无法恢复旧快照"
    return 1
  fi

  if ! persist_log_db_env "$dsn" "$persisted_log_network" "$LOG_ENV_SNAPSHOT_IDENTITY"; then
    restore_log_db_env_snapshot ||
      log_error "高危：.env 提交失败后无法恢复旧 .env 快照（可能存在并发修改）"
    restore_log_network_override_snapshot ||
      log_error "高危：.env 提交失败后无法恢复旧 Compose override"
    return 1
  fi
}

run_setup_compose_recreate() {
  local override="${PROJECT_DIR}/docker-compose.override.yml"
  local host_overlay="${PROJECT_DIR}/docker-compose.host.yml"
  local log_overlay="${PROJECT_DIR}/docker-compose.logdb.yml"
  local newapi_network log_network
  local -a compose_args=(--env-file "$ENV_FILE" -f "$COMPOSE_FILE")
  [[ -f "$ENV_FILE" && ! -L "$ENV_FILE" &&
    -f "$COMPOSE_FILE" && ! -L "$COMPOSE_FILE" ]] || return 1
  newapi_network="$(read_env_value NEWAPI_NETWORK)"
  if env_file_has_key NEWAPI_NETWORK && [[ -z "$newapi_network" ]]; then
    [[ -f "$host_overlay" && ! -L "$host_overlay" ]] || return 1
    compose_args+=(-f "$host_overlay")
  fi
  log_network="$(read_env_value LOG_NETWORK)"
  if [[ -n "$log_network" && "$log_network" != "$newapi_network" ]]; then
    [[ -f "$log_overlay" && ! -L "$log_overlay" ]] || return 1
    compose_args+=(-f "$log_overlay")
  fi
  if [[ -e "$override" || -L "$override" ]]; then
    [[ -f "$override" && ! -L "$override" ]] || return 1
    compose_args+=(-f "$override")
  fi
  (
    cd "$PROJECT_DIR"
    COMPOSE_FILE= docker compose "${compose_args[@]}" \
      up -d --force-recreate --wait --wait-timeout 180 "$TOOLS_CONTAINER"
  )
}

# Return 10 when the candidate restart failed but the old configuration and
# service were restored, and 20 when rollback or old-service recovery failed.
restart_log_db_services_transactionally() {
  local env_restored=true override_restored=true
  if run_setup_compose_recreate; then
    return 0
  fi

  log_error "候选日志库配置重建失败，开始恢复旧配置"
  restore_log_db_env_snapshot || env_restored=false
  restore_log_network_override_snapshot || override_restored=false
  if [[ "$env_restored" != "true" || "$override_restored" != "true" ]]; then
    log_error "高危：候选重建失败且无法完整恢复旧 .env/override"
    return 20
  fi
  if run_setup_compose_recreate; then
    log_warn "候选日志库配置未生效；旧配置与旧服务已恢复健康"
    return 10
  fi
  log_error "高危：旧配置已恢复，但旧服务也无法重新达到健康状态"
  return 20
}

#######################################
# 主流程
#######################################
MODE="${1:-}"

main() {
  need_cmd docker
  detect_docker_compose
  resolve_project_dir

  echo ""
  echo -e "${BLUE}========================================${NC}"
  echo -e "${BLUE}  NewAPI-Tool 日志分库（LOG_SQL_DSN）兼容${NC}"
  echo -e "${BLUE}========================================${NC}"
  echo ""

  # 1) 取 LOG_SQL_DSN
  local raw_dsn="${LOG_SQL_DSN:-}"
  if [[ -z "$raw_dsn" ]]; then
    local newapi="${NEWAPI_CONTAINER:-}"
    [[ -z "$newapi" ]] && newapi="$(detect_newapi_container || true)"
    [[ -n "$newapi" ]] || die "找不到 NewAPI 容器（可用 NEWAPI_CONTAINER=<名字> 指定，或用 LOG_SQL_DSN=<dsn> 直接给）"
    log_success "NewAPI 容器: $newapi"
    raw_dsn="$(docker_inspect_env_value "$newapi" 'LOG_SQL_DSN' || true)"
    if [[ -z "$raw_dsn" ]]; then
      log_warn "NewAPI 容器未设置 LOG_SQL_DSN —— 该实例没有把日志分库，无需本脚本。"
      log_info  "（本工具直接连主库即可读到 logs；当前日志为空多半是另有原因。）"
      exit 0
    fi
  fi
  log_success "检测到 LOG_SQL_DSN（原始内容与凭据已隐藏）"

  # 2) 解析。任何歧义都失败关闭，不能把 MySQL 或带选项的 URL
  # 静默重组为 PostgreSQL keyword DSN。
  local engine host port db
  detect_dsn_format "$raw_dsn" >/dev/null ||
    die "LOG_SQL_DSN 格式不受支持或无法安全解析"
  engine="$(extract_dsn_engine "$raw_dsn")" || die "无法识别 LOG_SQL_DSN 引擎"
  host="$(extract_dsn_host "$raw_dsn")" || die "无法读取 LOG_SQL_DSN host"
  port="$(extract_dsn_port "$raw_dsn")" || die "无法读取 LOG_SQL_DSN port"
  db="$(extract_dsn_dbname "$raw_dsn")" || die "无法读取 LOG_SQL_DSN dbname"
  [[ -n "$host" && -n "$db" ]] || die "无法解析 LOG_SQL_DSN（host/dbname 缺失；原始内容已隐藏）"
  if [[ -z "$port" ]]; then
    [[ "$engine" == "mysql" ]] && port="3306" || port="5432"
  fi

  # 3) 决定工具怎么连到日志库（与 deploy.sh 主库逻辑同款）
  local need_network="" rewrite_required=false
  if [[ "$host" == "127.0.0.1" || "$host" == "localhost" || "$host" == "::1" ]]; then
    local hit hnet hname hport
    hit="$(find_container_by_published_port "$port")"
    hnet="$(printf '%s' "$hit" | cut -f1)"
    hname="$(printf '%s' "$hit" | cut -f2)"
    hport="$(printf '%s' "$hit" | cut -f3)"
    if [[ -n "$hnet" && -n "$hname" ]]; then
      log_warn "日志库 127.0.0.1:${port} 实为容器 '${hname}'（端口仅发布在宿主机回环，网关不可达）"
      log_info "将把 $TOOLS_CONTAINER 接入网络 '${hnet}'，用容器名 '${hname}:${hport}' 直连"
      host="$hname"; port="$hport"; need_network="$hnet"; rewrite_required=true
    else
      host="host.docker.internal"
      rewrite_required=true
      log_info "日志库在宿主机回环上，改写为 host.docker.internal"
    fi
  elif is_ip_literal "$host"; then
    local hit hnet hname
    hit="$(find_container_by_network_ip "$host")"
    hnet="$(printf '%s' "$hit" | cut -f1)"
    hname="$(printf '%s' "$hit" | cut -f2)"
    if [[ -n "$hnet" && -n "$hname" ]]; then
      log_warn "日志库 ${host} 是容器 '${hname}' 在网络 '${hnet}' 上的 IP"
      log_info "将把 $TOOLS_CONTAINER 接入网络 '${hnet}'，用容器名直连"
      host="$hname"; need_network="$hnet"; rewrite_required=true
    else
      log_info "日志库地址 ${host} 是 IP 但不属于已知 docker 网络容器，按外部地址原样使用"
    fi
  else
    log_info "日志库地址为主机名/外部地址，原样使用: ${host}"
  fi

  local final_dsn="$raw_dsn"
  if [[ "$rewrite_required" == "true" ]]; then
    final_dsn="$(rewrite_dsn_host_port "$raw_dsn" "$host" "$port")" ||
      die "无法在保留凭据与连接选项的前提下改写 LOG_SQL_DSN"
  fi

  echo ""
  log_success "最终连接目标（用户名与密码已隐藏）:"
  echo -e "    ${GREEN}engine=${engine} host=${host} port=${port} dbname=${db} credentials=***${NC}"
  [[ -n "$need_network" ]] && echo -e "    需接入网络: ${GREEN}${need_network}${NC}"
  echo ""

  if [[ "$MODE" == "--print" ]]; then
    log_info "--print 模式：不修改任何文件 / 容器。"
    exit 0
  fi

  # 4) 一次性提交完整 .env。LOG_NETWORK 同时持久化，确保后续
  # install.sh/deploy.sh 仍会选择 docker-compose.logdb.yml，不会在升级后丢网。
  local persisted_log_network="$need_network"
  # 5) 先提交不含凭据的 override，再提交 .env。跨文件无法做到单次 rename，
  # 因此保存旧 override 快照：override 失败时 .env 尚未变化，.env 失败时
  # 原子恢复 override。进程在两次提交间崩溃也只会多接一条网络，重跑可收敛。
  commit_log_db_configuration "$final_dsn" "$persisted_log_network" ||
    die "日志库配置事务失败；旧 .env/override 已尝试恢复，可安全重跑"
  log_success "已写入 $ENV_FILE"

  if [[ "$MODE" == "--no-restart" ]]; then
    local manual_compose_files="-f docker-compose.yml"
    if [[ -n "$persisted_log_network" ]]; then
      ensure_tool_on_network "$persisted_log_network"
    fi
    if env_file_has_key NEWAPI_NETWORK && [[ -z "$(read_env_value NEWAPI_NETWORK)" ]]; then
      manual_compose_files+=" -f docker-compose.host.yml"
    fi
    if [[ -n "$(read_env_value LOG_NETWORK)" &&
      "$(read_env_value LOG_NETWORK)" != "$(read_env_value NEWAPI_NETWORK)" ]]; then
      manual_compose_files+=" -f docker-compose.logdb.yml"
    fi
    if [[ -f "${PROJECT_DIR}/docker-compose.override.yml" &&
      ! -L "${PROJECT_DIR}/docker-compose.override.yml" ]]; then
      manual_compose_files+=" -f docker-compose.override.yml"
    fi
    log_info "--no-restart：已写入 .env，未重建容器。稍后请手动："
    echo "    cd $PROJECT_DIR && docker compose --env-file .env ${manual_compose_files} up -d --force-recreate --wait $TOOLS_CONTAINER"
    exit 0
  fi

  # 7) 重建并等待语义健康；失败时恢复两份旧配置并尝试恢复旧服务。
  log_info "重建 $TOOLS_CONTAINER 以加载新配置..."
  local restart_status
  if restart_log_db_services_transactionally; then
    restart_status=0
  else
    restart_status=$?
  fi
  case "$restart_status" in
    0) ;;
    10) die "日志库配置重建失败，已恢复旧配置与旧服务；可修正后安全重跑" ;;
    *) die "高危：日志库配置重建与旧服务恢复均失败，请立即检查 docker compose ps/logs" ;;
  esac

  echo ""
  log_success "完成！日志类查询（仪表盘流量、使用日志、模型监控、风控/IP 分析）现在会读取日志库。"
  log_info "验证： $DOCKER_COMPOSE logs --tail=20 $TOOLS_CONTAINER  并刷新前端仪表盘。"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
