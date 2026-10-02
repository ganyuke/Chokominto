#!/bin/sh
# Boot an emulated Raspberry Pi 4 (4x Cortex-A72, 4 GB, Debian 13 arm64) in
# podman, for trying installs, updates and both scrobblers before the real
# Pi. Needs only podman and curl on the host.
#
# The disk, SSH key and downloads live in $PI_SIM_DIR (default
# ~/.cache/chokominto-pi-sim). Delete disk.qcow2 there for a fresh Pi.
# Queries run about 20 times slower than natively, so use it to check that
# things work, not for timings.
set -e
here=$(cd "$(dirname "$0")" && pwd)
work=${PI_SIM_DIR:-$HOME/.cache/chokominto-pi-sim}
mkdir -p "$work/seed"
cd "$work"

podman build -q -t chokominto-pi-sim "$here" >/dev/null
if [ ! -f debian-13-arm64.qcow2 ]; then
	url=https://cloud.debian.org/images/cloud/trixie/latest
	curl -fsSLo debian-13-arm64.qcow2.part "$url/debian-13-genericcloud-arm64.qcow2"
	curl -fsSL "$url/SHA512SUMS" | grep ' debian-13-genericcloud-arm64.qcow2$' |
		sed 's/debian-13-genericcloud-arm64.qcow2/debian-13-arm64.qcow2.part/' | sha512sum -c --quiet
	mv debian-13-arm64.qcow2.part debian-13-arm64.qcow2
fi
[ -f id_pi ] || ssh-keygen -q -t ed25519 -N '' -f id_pi -C pi-sim
printf 'instance-id: chokominto-pi-1\nlocal-hostname: raspberrypi\n' > seed/meta-data
cat > seed/user-data <<EOT
#cloud-config
users:
  - name: pi
    groups: [sudo]
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - $(cat id_pi.pub)
growpart: {mode: auto, devices: ['/']}
EOT
if [ ! -f disk.qcow2 ]; then
	cp debian-13-arm64.qcow2 disk.qcow2
	rm -f vars.raw
	podman run --rm -v "$work:/pi:Z" chokominto-pi-sim qemu-img resize -q /pi/disk.qcow2 16G
fi
cp "$here/run-vm.sh" .
podman rm -f chokominto-pi >/dev/null 2>&1 || true
podman run -d --name chokominto-pi -p 127.0.0.1:2222:22 -p 127.0.0.1:3939:3939 \
	-v "$work:/pi:Z" chokominto-pi-sim /pi/run-vm.sh >/dev/null

ssh="ssh -i $work/id_pi -p 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null pi@127.0.0.1"
echo "Booting. The first boot takes a few minutes."
until $ssh -q -o ConnectTimeout=5 'cloud-init status --wait' >/dev/null 2>&1; do sleep 5; done
cat <<EOT
The Pi is up. Log in with:
  $ssh
Copy files with scp -P 2222 -i $work/id_pi. Its port 3939 is at http://127.0.0.1:3939.
Then follow the README's install steps on it. To try scrobblers against it:
  python3 $here/clients.py http://127.0.0.1:3939 <user> <pano token> <web scrobbler token> --stats --pages
A made-up Maloja history to import: python3 $here/make_maloja.py malojadb.sqlite 80000
Stop it with: podman stop chokominto-pi
EOT
