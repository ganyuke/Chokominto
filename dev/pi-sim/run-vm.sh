#!/bin/sh
# Emulated Raspberry Pi 4: 4x Cortex-A72, 4 GB RAM, arm64 Debian.
set -e
cd /pi
[ -f vars.raw ] || cp /usr/share/edk2/aarch64/vars-template-pflash.raw vars.raw
xorriso -as mkisofs -quiet -o seed.iso -V cidata -J -r seed
exec qemu-system-aarch64 -M virt -cpu cortex-a72 -smp 4 -m 4096 -accel tcg,thread=multi \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/edk2/aarch64/QEMU_EFI-pflash.raw \
  -drive if=pflash,format=raw,file=vars.raw \
  -drive if=virtio,format=qcow2,file=disk.qcow2 \
  -drive if=virtio,format=raw,file=seed.iso,readonly=on \
  -netdev user,id=n0,hostfwd=tcp::22-:22,hostfwd=tcp::3939-:3939 -device virtio-net-pci,netdev=n0 \
  -nographic -serial mon:stdio
