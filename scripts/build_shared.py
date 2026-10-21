"""Build existing gomobile bindings plus a CLI sharing their Go runtime (Python 3.10+)."""
import argparse
import json
import os
import pathlib
import platform
import re
import shutil
import subprocess
import tempfile
import zipfile

ARCHES = {
    'arm': ('armeabi-v7a', 'armv7a-linux-androideabi'),
    'arm64': ('arm64-v8a', 'aarch64-linux-android'),
    '386': ('x86', 'i686-linux-android'),
    'amd64': ('x86_64', 'x86_64-linux-android'),
}
LDFLAGS = '-s -w -buildid= -checklinkname=0'


def rename_function(source, old, new):
    pattern = f'func {old}() {{'
    if source.count(pattern) != 1:
        raise ValueError(f'Upstream changed: expected exactly one {pattern}')
    return source.replace(pattern, f'func {new}() {{')


def replace_once(source, old, new):
    if source.count(old) != 1:
        raise ValueError(f'Upstream changed: expected exactly one {old!r}')
    return source.replace(old, new)


def build_system_dns_helper(repo, work, env):
    sdk = pathlib.Path(env['ANDROID_HOME']).resolve()
    android_jar = sdk / 'platforms/android-37.0/android.jar'
    d8 = sdk / 'build-tools/37.0.0/lib/d8.jar'
    for required in (android_jar, d8):
        if not required.is_file():
            raise ValueError(f'Android SDK component missing: {required}')
    suffix = '.exe' if os.name == 'nt' else ''
    java_home = env.get('JAVA_HOME')
    def java_tool(name):
        return pathlib.Path(java_home) / 'bin' / (name + suffix) if java_home else name + suffix
    classes = work / 'system-dns-classes'
    dex = work / 'system-dns-dex'
    for directory in (classes, dex):
        shutil.rmtree(directory, ignore_errors=True)
        directory.mkdir()
    run([java_tool('javac'), '--release', '8', '-classpath', android_jar,
         '-d', classes, *sorted((repo / 'native/systemdns/java').rglob('*.java'))], repo, env)
    run([java_tool('java'), '-cp', d8, 'com.android.tools.r8.D8', '--release',
         '--min-api', '26', '--lib', android_jar, '--output', dex,
         *sorted(classes.rglob('*.class'))], repo, env)
    if sorted(path.name for path in dex.iterdir()) != ['classes.dex']:
        raise ValueError('System DNS helper must fit in one dex')
    return dex / 'classes.dex'


def package_aar(baseline, target, artifacts):
    replacements = {}
    for abi, (core, launcher) in artifacts.items():
        replacements[f'jni/{abi}/libgojni.so'] = core
        replacements[f'jni/{abi}/libxray.so'] = launcher
    with zipfile.ZipFile(baseline) as original, zipfile.ZipFile(target, 'w', zipfile.ZIP_DEFLATED) as result:
        expected = {f'jni/{abi}/libgojni.so' for abi in artifacts}
        actual = {name for name in original.namelist() if name.startswith('jni/') and name.endswith('.so')}
        if actual != expected:
            raise ValueError(f'Unexpected gomobile native entries: {actual}')
        for entry in original.infolist():
            if entry.filename not in replacements:
                result.writestr(entry, original.read(entry))
        for name, path in replacements.items():
            entry = zipfile.ZipInfo(name)
            entry.compress_type = zipfile.ZIP_DEFLATED
            entry.external_attr = 0o100755 << 16
            result.writestr(entry, path.read_bytes())


def run(command, cwd, env, log=None):
    print('+ ' + ' '.join(map(str, command)), flush=True)
    result = subprocess.run(list(map(str, command)), cwd=cwd, env=env, text=True, errors='replace',
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if log:
        pathlib.Path(log).write_text(result.stdout, encoding='utf-8')
    if result.returncode:
        raise RuntimeError(result.stdout)
    return result.stdout.strip()


def build(args, work):
    repo = pathlib.Path(__file__).resolve().parents[1]
    env = os.environ.copy()
    for name in ('GOOS', 'GOARCH', 'GOARM', 'CC', 'CXX', 'CGO_ENABLED'):
        env.pop(name, None)
    ndk = pathlib.Path(env['ANDROID_NDK_HOME']).resolve()
    host = {'Windows': 'windows-x86_64', 'Linux': 'linux-x86_64', 'Darwin': 'darwin-x86_64'}[platform.system()]
    suffix = '.exe' if os.name == 'nt' else ''
    clang = ndk / 'toolchains/llvm/prebuilt' / host / ('bin/clang' + suffix)
    if not clang.is_file():
        raise ValueError(f'NDK clang missing: {clang}')
    tools, temp = work / 'tools', work / 'tmp'
    tools.mkdir(exist_ok=True)
    temp.mkdir(exist_ok=True)
    env.update(GOBIN=str(tools), TEMP=str(temp), TMP=str(temp), TMPDIR=str(temp))
    env['PATH'] = str(tools) + os.pathsep + env['PATH']
    mobile = run(['go', 'list', '-m', '-f', '{{.Version}}', 'golang.org/x/mobile'], repo, env)
    for tool in ('gomobile', 'gobind'):
        run(['go', 'install', f'golang.org/x/mobile/cmd/{tool}@{mobile}'], repo, env)
    baseline = work / 'baseline.aar'
    output = run([tools / ('gomobile' + suffix), 'bind', '-work', '-x', '-target',
                  ','.join('android/' + arch for arch in args.arch), '-androidapi', str(args.api),
                  '-trimpath', '-ldflags=' + LDFLAGS, '-o', baseline, '.'], repo, env, work / 'gomobile.log')
    matches = re.findall(r'^WORK=(.+)$', output, re.MULTILINE)
    if not matches:
        raise ValueError('gomobile did not report its work directory')
    generated = pathlib.Path(matches[-1].strip())
    core = pathlib.Path(run(['go', 'list', '-m', '-f', '{{.Dir}}', 'github.com/xtls/xray-core'], repo, env))
    helper_dex = build_system_dns_helper(repo, work, env)
    artifacts = {}
    for arch in args.arch:
        abi, triple = ARCHES[arch]
        module = generated / ('src-android-' + arch)
        source = module / 'gobind'
        for filename, old, new in [('main.go', 'main', 'xrayCLIMain'), ('run.go', 'init', 'initXrayCLI')]:
            text = rename_function((core / 'main' / filename).read_text(), old, new)
            if filename == 'run.go':
                # Configuration tests start no observer; a running service
                # starts it before loading configuration.
                anchor = '\tprintVersion()\n\tserver, err := startXray()\n'
                text = replace_once(text, anchor, '\tprintVersion()\n\tif !*test {\n'
                                    '\t\tdefer startSystemDNS()()\n\t}\n\tserver, err := startXray()\n')
            (source / ('xray_' + filename)).write_text(text, encoding='utf-8')
        # The CLI's system DNS package lives in gomobile's module, keeping the
        # generated dex out of the checkout and the package out of the JNI API.
        systemdns = module / 'systemdns'
        systemdns.mkdir(exist_ok=True)
        for file in (repo / 'native/systemdns').glob('*.go'):
            if not file.name.endswith('_test.go'):
                shutil.copyfile(file, systemdns / file.name)
        shutil.copyfile(helper_dex, systemdns / 'helper.dex')
        shutil.copyfile(core / 'main/version.go', source / 'xray_version.go')
        shutil.copyfile(repo / 'native/cli/export.go.txt', source / 'cli_export.go')
        native = work / abi
        native.mkdir(exist_ok=True)
        target = f'--target={triple}{args.api}'
        android_env = dict(env, GOOS='android', GOARCH=arch, CGO_ENABLED='1',
                           CC=f'"{clang}" {target}', GOARM='7')
        library, launcher = native / 'libgojni.so', native / 'libxray.so'
        run(['go', 'build', '-trimpath', '-ldflags=' + LDFLAGS, '-buildmode=c-shared',
             '-o', library, './gobind'], module, android_env, native / 'build.log')
        run([clang, target, '-O2', '-fPIE', '-pie', '-Wall', '-Wextra', '-Werror',
             '-Wl,-z,max-page-size=16384', '-o', launcher, repo / 'native/cli/launcher.c', '-ldl'], repo, env)
        artifacts[abi] = (library, launcher)
    target = pathlib.Path(args.output).resolve()
    target.parent.mkdir(parents=True, exist_ok=True)
    staged = work / 'shared.aar'
    package_aar(baseline, staged, artifacts)
    # Build fully before replacing the caller's outputs.
    shutil.copyfile(staged, target)
    shutil.copyfile(work / 'baseline-sources.jar', target.with_name(target.stem + '-sources.jar'))
    (work / 'build-info.json').write_text(json.dumps({'gomobile_work': str(generated), 'abis': list(artifacts),
        'mobile': mobile, 'core': str(core), 'output': str(target)}, indent=2))
    print(f'Built {target}', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--arch', nargs='+', choices=ARCHES, default=list(ARCHES))
    parser.add_argument('--api', type=int, default=24)
    parser.add_argument('--output', default='libv2ray.aar')
    parser.add_argument('--work-dir', help='Retain generated sources and logs here')
    args = parser.parse_args()
    if args.api < 24 or len(args.arch) != len(set(args.arch)):
        parser.error('API must be >= 24 and architectures must be unique')
    if args.work_dir:
        work = pathlib.Path(args.work_dir).resolve()
        work.mkdir(parents=True, exist_ok=True)
        build(args, work)
    else:
        with tempfile.TemporaryDirectory(prefix='xray-shared-') as temp:
            build(args, pathlib.Path(temp))


if __name__ == '__main__':
    main()
