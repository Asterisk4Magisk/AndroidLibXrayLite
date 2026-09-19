# AndroidLibXrayLite

## Build requirements
* JDK
* Go
* Android NDK (`ANDROID_NDK_HOME`), SDK (`ANDROID_HOME`)
* Python 3.10+ (standard library only)

## Build instructions
1. `git clone [repo] && cd AndroidLibXrayLite`
2. `python3 scripts/build_shared.py`

The build installs gomobile/gobind at the version pinned in go.mod into a temporary
directory. Existing Java/JNI interfaces and assets are preserved. Outputs are
`libv2ray.aar` and `libv2ray-sources.jar`. All four Android ABIs are built at API 24.
Use `--arch amd64` for one ABI, `--output <file.aar>` to change the destination, or
`--work-dir <directory>` to retain generated sources and build logs. On Windows,
use `python`.

## Shared native CLI

Each AAR ABI directory contains:

- `libgojni.so`: one Go runtime and Xray Core, exposing both JNI and CLI entry points.
- `libxray.so`: a small native executable, packaged with a `.so` suffix for Android.

Execute `libxray.so` directly from the application's installed native library directory:

```sh
/absolute/native/library/dir/libxray.so version
/absolute/native/library/dir/libxray.so run -config /absolute/config.json
```

The launcher resolves `libgojni.so` beside its own executable using `/proc/self/exe`,
regardless of the working directory. No copying to the configuration directory is needed.
The Android app must enable native library extraction (for example AGP `useLegacyPackaging = true`)
so the executable exists on disk. The launcher forwards Xray arguments unchanged.

For an explicit alternative location, set `ANDROID_XRAY_LIBRARY` to an absolute
library path. Relative paths are rejected. The library must expose CLI ABI v1;
use the launcher and library from the same release. Loading failures return 70,
an incompatible library returns 71, and an invalid path override returns 64.
Xray commands preserve their own output, exit codes and signal handling.

JNI continues to use the existing mobile initialization and logcat behavior. The CLI
restores stdout/stderr after Go initialization so its parent can capture output.
It also imports the process environment into Go, including `XRAY_LOCATION_ASSET`;
set this to the directory containing `geoip.dat` and `geosite.dat`.
The CLI entry is exclusively for a standalone process and must never be called inside
the app's JVM: upstream CLI commands may terminate their process.
