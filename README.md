# AndroidLibXrayLite

## Build requirements
* JDK 21+ (`JAVA_HOME`)
* Go
* Android NDK (`ANDROID_NDK_HOME`), SDK (`ANDROID_HOME`) with platform 37.0 and Build Tools 37.0.0
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

## ROOT system DNS integration

Xray's `localhost` DNS uses Go's default resolver. On Android that asks netd,
whose queries are not owned by the core and can re-enter ROOT interception.
A running `run` service (not `-test`) with effective UID 0 therefore starts its
own Java network observer; JNI and other CLI commands do not. The observer uses
ConnectivityManager / LinkProperties to supply physical-network DNS addresses.
The core's default resolver then sends each UDP query to all of them from its
own sockets (TCP tries them in order). This covers `localhost` DNS, freedom and
dial-time lookups. A TUN inbound's `autoOutboundsInterface` resolver keeps
upstream behavior.

The build compiles `native/systemdns/java` with javac and SDK D8, then embeds
the single dex in `libgojni.so`. At startup the core verifies and reuses
`<working-directory>/system-dns-helper.dex`, or atomically replaces it from the
embedded bytes. The file is root-owned, mode 0400, and retained at exit with any
ART cache.

An anonymous socketpair carries version 1 address snapshots directly to the core
as fd 0 and fd 3; no named socket is created. Helper failure, including loss of
the connectivity service when system_server restarts, clears DNS and shuts the
core down gracefully with status 70.

Host tests: `go test ./native/systemdns` (file-cache and resolver tests need Linux
or Android). AVD regression: `tools/test_root_system_dns_core.py`.
