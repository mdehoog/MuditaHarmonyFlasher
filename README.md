# Mudita Harmony Flasher

Small Golang utility that can be used to flash custom firmware on a Mudita Harmony device.

## How it works

[MuditaOS](https://github.com/mudita/MuditaOS) is open source.
However, Mudita doesn't provide a way to build a full image for the device due to device assets existing in a separate private repository, for licensing reasons.
This utility simply downloads a recent firmware from the official Mudita S3 bucket, and patches the `bin/os.bin` file, leaving everything else untouched.
It then uses the same process that [Mudita Center](https://github.com/mudita/mudita-center/) uses to flash the custom firmware.

## Usage

Building `os.bin`:
```
git clone --recurse-submodules git@github.com:mudita/MuditaOS.git
docker pull wearemudita/mudita_os_builder:latest
docker run --platform linux/amd64 --entrypoint bash -it -v $(pwd):/mnt wearemudita/mudita_os_builder:latest
cd /mnt
./configure.sh BellHybrid rt1051 release
cd build-BellHybrid-rt1051-Release
mkdir -p sysroot/system_a/bin
make BellHybrid-boot.bin
```

Flashing `os.bin`:
```
go run github.com/mdehoog/MuditaHarmonyFlasher ./build-BellHybrid-rt1051-Release/sysroot/system_a/bin/os.bin
```
