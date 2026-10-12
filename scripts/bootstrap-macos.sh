#!/bin/sh
set -eu

if [ "$(uname -s)" != "Darwin" ]; then
  echo "This bootstrap is for macOS; use Docker Engine and Compose on Linux." >&2
  exit 1
fi
if ! command -v brew >/dev/null 2>&1; then
  echo "Install Homebrew first: https://brew.sh/" >&2
  exit 1
fi

brew install go docker docker-compose colima cloudflared python

profile="${COLIMA_PROFILE:-ai-workflow}"
if ! colima status --profile "$profile" >/dev/null 2>&1; then
  colima start --profile "$profile" --cpu 6 --memory 8 --disk 40
fi
set_vm_dns() {
  nameserver="$1"
  colima ssh --profile "$profile" -- sudo sh -c \
    "rm -f /etc/resolv.conf && printf 'nameserver %s\\n' '$nameserver' > /etc/resolv.conf && chmod 644 /etc/resolv.conf"
}
if ! colima ssh --profile "$profile" -- getent hosts registry-1.docker.io >/dev/null 2>&1; then
  set_vm_dns 1.1.1.1
fi
if ! colima ssh --profile "$profile" -- getent hosts registry-1.docker.io >/dev/null 2>&1; then
  set_vm_dns 8.8.8.8
fi
colima ssh --profile "$profile" -- getent hosts registry-1.docker.io >/dev/null
docker context use "colima-$profile"
docker info >/dev/null
echo "Colima profile $profile is ready with 6 CPUs, 8 GiB memory, and a 40 GiB disk."
