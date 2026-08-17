#!/usr/bin/env bash
set -euo pipefail

# alistbatch cross-build script — mirrors .github/workflows/build.yml matrix
# Usage:
#   ./build.sh                          # version from git describe
#   ./build.sh --version v0.1.0         # explicit version
#   ./build.sh --outdir dist            # custom output dir (default: dist)
#   ./build.sh --skip-tests             # skip go vet / go test
#   ./build.sh --targets linux/amd64,darwin/arm64  # build subset
#   VERSION=v0.1.0 ./build.sh           # env var also works

OUTDIR="dist"
SKIP_TESTS=0
VERSION="${VERSION:-}"
TARGETS=""

usage() {
  cat <<'EOF'
Usage: ./build.sh [options]

Options:
  --version <ver>   Set version string (ldflags -X main.version). Default: git describe or "dev"
  --outdir <dir>    Output directory (default: dist)
  --skip-tests      Skip go vet and go test
  --targets <list>  Comma-separated GOOS/GOARCH[/GOARM] subset (e.g. linux/amd64,darwin/arm64,linux/arm/7)
  -h, --help        Show this help

Env:
  VERSION           Same as --version
  OUTDIR            Same as --outdir

Examples:
  ./build.sh
  ./build.sh --version v0.1.0
  ./build.sh --targets linux/amd64,windows/amd64
  ./build.sh --skip-tests --outdir ./out
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --version=*) VERSION="${1#*=}"; shift ;;
    --outdir) OUTDIR="$2"; shift 2 ;;
    --outdir=*) OUTDIR="${1#*=}"; shift ;;
    --skip-tests) SKIP_TESTS=1; shift ;;
    --targets) TARGETS="$2"; shift 2 ;;
    --targets=*) TARGETS="${1#*=}"; shift ;;
    -h|--help) usage; exit 0 ;;
    --) shift; break ;;
    -*) echo "unknown flag: $1" >&2; usage >&2; exit 1 ;;
    *) echo "unknown arg: $1" >&2; usage >&2; exit 1 ;;
  esac
done

if ! command -v go >/dev/null 2>&1; then
  echo "error: go not found in PATH" >&2
  exit 1
fi

# Resolve version like CI does
if [[ -z "$VERSION" ]]; then
  if [[ "${GITHUB_REF:-}" == refs/tags/* ]]; then
    VERSION="${GITHUB_REF_NAME:-dev}"
  else
    VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
    # fallback if not a git repo
    if [[ "$VERSION" == "dev-unknown" || "$VERSION" == "-unknown" ]]; then
      VERSION="dev"
    fi
  fi
fi

echo "==> alistbatch cross-build"
echo "    version: $VERSION"
echo "    outdir:  $OUTDIR"
echo "    go:      $(go version)"

if [[ "$SKIP_TESTS" -eq 0 ]]; then
  echo "==> go vet ./..."
  go vet ./...
  echo "==> go test ./... -count=1"
  go test ./... -count=1
else
  echo "==> skipping vet/test (--skip-tests)"
fi

mkdir -p "$OUTDIR"

# Full matrix — same as .github/workflows/build.yml
ALL_TARGETS=(
  "linux/amd64"
  "linux/386"
  "linux/arm64"
  "linux/arm/7"
  "windows/amd64"
  "windows/386"
  "windows/arm64"
  "darwin/amd64"
  "darwin/arm64"
)

# Filter if --targets given
if [[ -n "$TARGETS" ]]; then
  IFS=',' read -ra _WANT <<< "$TARGETS"
  FILTERED=()
  for t in "${ALL_TARGETS[@]}"; do
    for w in "${_WANT[@]}"; do
      w="$(echo "$w" | xargs)" # trim
      if [[ "$t" == "$w" ]]; then
        FILTERED+=("$t")
        break
      fi
    done
  done
  if [[ ${#FILTERED[@]} -eq 0 ]]; then
    echo "error: --targets matched nothing. Available: ${ALL_TARGETS[*]}" >&2
    exit 1
  fi
  ALL_TARGETS=("${FILTERED[@]}")
  echo "==> filtered targets: ${ALL_TARGETS[*]}"
fi

LDFLAGS="-s -w -X main.version=${VERSION}"
FAILED=0
BUILT=0

for spec in "${ALL_TARGETS[@]}"; do
  IFS='/' read -r GOOS GOARCH GOARM <<< "$spec"
  # GOARM is empty for most; only linux/arm/7 has it
  SUFFIX=""
  if [[ "$GOARCH" == "arm" && -n "${GOARM:-}" ]]; then
    SUFFIX="v${GOARM}"
  fi
  ASSET="alistbatch-${GOOS}-${GOARCH}${SUFFIX}"
  if [[ "$GOOS" == "windows" ]]; then
    ASSET="${ASSET}.exe"
  fi
  OUT="$OUTDIR/$ASSET"

  echo "==> building $ASSET (GOOS=$GOOS GOARCH=$GOARCH GOARM=${GOARM:-} version=$VERSION)"
  if ! env CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" GOARM="${GOARM:-}" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$OUT" .; then
    echo "    FAIL $ASSET" >&2
    FAILED=$((FAILED+1))
    continue
  fi
  ls -lh "$OUT"
  # checksum (sha256sum on linux, shasum on macOS)
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$OUTDIR" && sha256sum "$ASSET" > "$ASSET.sha256")
  else
    (cd "$OUTDIR" && shasum -a 256 "$ASSET" > "$ASSET.sha256")
  fi
  cat "$OUTDIR/$ASSET.sha256"
  BUILT=$((BUILT+1))
done

echo ""
echo "==> done: $BUILT built, $FAILED failed, out: $OUTDIR/"
ls -lh "$OUTDIR" 2>/dev/null || true
if [[ "$FAILED" -gt 0 ]]; then
  exit 1
fi
